package k8spoc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type fakeJobs struct {
	mu             sync.Mutex
	jobs           map[string]kubeJob
	lostCreate     bool
	deleteFailOnce bool
}

func (f *fakeJobs) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := strings.TrimPrefix(r.URL.Path, "/apis/batch/v1/namespaces/fullsend-runner/jobs/")
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case "POST":
		var input struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		job := kubeJob{}
		job.Metadata.Name = input.Metadata.Name
		job.Metadata.UID = "uid-" + input.Metadata.Name
		job.Metadata.Labels = input.Metadata.Labels
		f.jobs[input.Metadata.Name] = job
		if f.lostCreate {
			f.lostCreate = false
			http.Error(w, "response lost", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(job)
	case "GET":
		job, ok := f.jobs[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(job)
	case "DELETE":
		if f.deleteFailOnce {
			f.deleteFailOnce = false
			http.Error(w, "temporary delete failure", 500)
			return
		}
		var input struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		job, ok := f.jobs[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if input.Preconditions.UID != job.Metadata.UID {
			http.Error(w, "UID mismatch", 409)
			return
		}
		delete(f.jobs, name)
		w.WriteHeader(200)
	}
}

func (f *fakeJobs) finish(name string, success bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[name]
	if success {
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = append(job.Status.Conditions, kubeJobCondition{Type: "Failed", Status: "True"})
	}
	f.jobs[name] = job
}

func TestGatewayAttemptKeepsReservationUntilIndependentStop(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "poc_gateway_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	t.Setenv("DATABASE_URL", u.String())
	if err := Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.json")
	selectionPath := filepath.Join(dir, "selection.json")
	fixture := `{"tenant":"fixture-tenant","source":"fixture-source","cursor":"c1","observations":[
		{"key":"one","entity":"one","revision":"1","kind":"gateway_demo","behavior":"success","authorized":true},
		{"key":"two","entity":"two","revision":"1","kind":"gateway_demo","behavior":"success","authorized":true}]}`
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selectionPath, []byte(`{"by_kind":{"gateway_demo":["gateway-demo"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal(err)
	}
	conn, err := connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	fake := &fakeJobs{jobs: map[string]kubeJob{}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	k := &kubeClient{client: server.Client(), host: server.URL, token: "fixture", namespace: "fullsend-runner", image: "stub-image", gatewayImage: "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef"}
	fake.lostCreate = true
	if err := dispatchWithClient(ctx, conn, k); err == nil {
		t.Fatal("lost gateway Job create response was treated as certain success")
	}
	if err := Cancel(ctx, 1); err == nil {
		t.Fatal("accepted cancellation before Job creation was reconciled")
	}
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	fake.finish("fs-poc-1-1", true)
	fake.mu.Lock()
	delete(fake.jobs, "fs-poc-2-1") // Simulate a runner Job disappearing without terminal evidence.
	fake.mu.Unlock()
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var reserved int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM selected_runs WHERE state='reserved'`).Scan(&reserved); err != nil || reserved != 2 {
		t.Fatalf("reservations released before remote cleanup: count=%d err=%v", reserved, err)
	}
	fake.mu.Lock()
	firstCleanup := fake.jobs["fs-clean-1-1"]
	firstCleanup.Metadata.Labels["fullsend.ai/attempt"] = "foreign"
	fake.jobs["fs-clean-1-1"] = firstCleanup
	fake.mu.Unlock()
	if err := dispatchWithClient(ctx, conn, k); err == nil {
		t.Fatal("accepted cleanup Job with a foreign attempt label")
	}
	fake.mu.Lock()
	firstCleanup.Metadata.Labels["fullsend.ai/attempt"] = "1"
	firstCleanup.Status.Failed = 1 // One failed Pod is retriable; the Job is not terminal.
	fake.jobs["fs-clean-1-1"] = firstCleanup
	fake.mu.Unlock()
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal("treated a retriable cleanup Pod failure as terminal:", err)
	}
	fake.finish("fs-clean-1-1", true)
	fake.finish("fs-clean-2-1", false)
	if err := dispatchWithClient(ctx, conn, k); err == nil {
		t.Fatal("failed cleanup Job did not hold its reservation")
	}
	var succeeded, failed int
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM selected_runs WHERE state='succeeded'),(SELECT count(*) FROM selected_runs WHERE state='failed')`).Scan(&succeeded, &failed); err != nil || succeeded != 0 || failed != 0 {
		t.Fatalf("terminal runner released reservation before Job deletion: succeeded=%d failed=%d err=%v", succeeded, failed, err)
	}
	if err := dispatchWithClient(ctx, conn, k); err == nil {
		t.Fatal("failed cleanup Job did not hold its reservation")
	}
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM selected_runs WHERE state='succeeded'),(SELECT count(*) FROM selected_runs WHERE state='failed')`).Scan(&succeeded, &failed); err != nil || succeeded != 1 || failed != 0 {
		t.Fatalf("unexpected outcomes before retry: succeeded=%d failed=%d err=%v", succeeded, failed, err)
	}
	fake.mu.Lock()
	delete(fake.jobs, "fs-clean-2-1")
	fake.mu.Unlock()
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	fake.finish("fs-clean-2-1", true)
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM selected_runs WHERE state='succeeded'),(SELECT count(*) FROM selected_runs WHERE state='failed')`).Scan(&succeeded, &failed); err != nil || succeeded != 1 || failed != 1 {
		t.Fatalf("unexpected final outcomes: succeeded=%d failed=%d err=%v", succeeded, failed, err)
	}
	if err := os.WriteFile(fixturePath, []byte(`{"tenant":"fixture-tenant","source":"fixture-source","cursor":"c2","observations":[{"key":"three","entity":"three","revision":"1","kind":"gateway_demo","behavior":"success","authorized":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal(err)
	}
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(ctx, 0); err == nil {
		t.Fatal("accepted zero run ID for cancellation")
	}
	if err := Cancel(ctx, 1); err == nil {
		t.Fatal("accepted cancellation of a completed run")
	}
	if err := Cancel(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(ctx, 3); err == nil {
		t.Fatal("accepted duplicate cancellation")
	}
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM selected_runs WHERE state='reserved'`).Scan(&reserved); err != nil || reserved != 1 {
		t.Fatalf("cancellation released remote reservation early: %d %v", reserved, err)
	}
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	fake.finish("fs-clean-3-1", true)
	if err := dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM selected_runs WHERE state='failed'`).Scan(&failed); err != nil || failed != 2 {
		t.Fatalf("cancellation did not finish as cleaned-up failure: %d %v", failed, err)
	}
	if err := completeGatewayAttempt(ctx, conn, attempt{id: 3, runID: 3}); err == nil {
		t.Fatal("repeated completion accepted an attempt no longer awaiting cleanup")
	}
	if _, err := conn.Exec(ctx, `UPDATE attempts SET state='cleanup',outcome='' WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	if err := completeGatewayAttempt(ctx, conn, attempt{id: 3, runID: 3}); err == nil {
		t.Fatal("cleanup without a terminal outcome released reservation")
	}
	if _, err := conn.Exec(ctx, `UPDATE attempts SET outcome='failed' WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	if err := completeGatewayAttempt(ctx, conn, attempt{id: 3, runID: 3}); err == nil {
		t.Fatal("cleanup completed without a reserved run")
	}
}

func TestRealJiraAttemptDeletesEventAfterVerifiedCleanup(t *testing.T) {
	ctx, conn := liveDB(t)
	config, _ := json.Marshal(LiveConfig{JiraBaseURL: "https://stage.example", Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo", Harnesses: []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}}, FullsendHarnesses: []string{"triage"}, RepoBundle: "fullsend-live-repo-0123456789abcdef", RepoRevision: strings.Repeat("a", 40), GCPAudience: "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/provider", VertexProject: "vertex-project"})
	if _, err := conn.Exec(ctx, `INSERT INTO config_revisions(digest,content) VALUES('real-jira-config',$1)`, config); err != nil {
		t.Fatal(err)
	}
	var observationID int64
	if err := conn.QueryRow(ctx, `INSERT INTO observations(tenant,source,observation_key,entity,revision,kind,behavior,authorized,config_digest) VALUES('owner/repo','jira:FSENDAI:fullsend-dispatcher-test','one','FSENDAI-1','r1','opened','success',true,'real-jira-config') RETURNING id`).Scan(&observationID); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := conn.QueryRow(ctx, `INSERT INTO selected_runs(observation_id,harness,execution,event_payload,state) VALUES($1,'triage','fullsend','{"source":{"system":"jira"}}','reserved') RETURNING id`, observationID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	var attemptID int64
	if err := conn.QueryRow(ctx, `INSERT INTO attempts(run_id,number,job_name,job_uid,state) VALUES($1,1,'fs-poc-77-1','runner-uid','active') RETURNING id`, runID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	deletedEvent := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/configmaps/fullsend-event-") && r.Method == http.MethodDelete {
			deletedEvent++
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/jobs/fs-clean-") && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"metadata":{"name":"fs-clean-77-1","labels":{"fullsend.ai/attempt":"%d","fullsend.ai/gateway-job":"attempt-cleanup"}},"status":{"succeeded":1}}`, attemptID)))
			return
		}
		if strings.Contains(r.URL.Path, "/jobs/fs-poc-") && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	k := &kubeClient{client: server.Client(), host: server.URL, namespace: "fullsend-runner"}
	a := attempt{id: attemptID, runID: runID, name: "fs-poc-77-1", uid: "runner-uid", state: "active", source: "jira:FSENDAI:fullsend-dispatcher-test", harness: "triage", execution: "fullsend"}
	var job kubeJob
	job.Metadata.Name, job.Metadata.UID = a.name, a.uid
	job.Metadata.Labels = map[string]string{"fullsend.ai/attempt": fmt.Sprint(attemptID), "fullsend.ai/gateway-job": "demo"}
	job.Status.Succeeded = 1
	if err := reconcileGatewayAttempt(ctx, conn, k, a, job, true); err != nil {
		t.Fatal(err)
	}
	a.state = "cleanup"
	if err := reconcileGatewayAttempt(ctx, conn, k, a, kubeJob{}, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := conn.QueryRow(ctx, `SELECT state FROM selected_runs WHERE id=$1`, runID).Scan(&state); err != nil || state != "succeeded" || deletedEvent != 2 {
		t.Fatalf("cleanup result state=%q event-deletes=%d err=%v", state, deletedEvent, err)
	}
}

func TestDurableFixtureCycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "poc_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Setenv("DATABASE_URL", u.String())
	if err = Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx); err != nil {
		t.Fatal("migration is not idempotent:", err)
	}
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.json")
	selectionPath := filepath.Join(dir, "selection.json")
	fixture := `{"tenant":"test","source":"fixture","cursor":"c1","observations":[
		{"key":"e-v1","entity":"e","revision":"1","kind":"opened","behavior":"success","authorized":true},
		{"key":"e-v2","entity":"e","revision":"2","kind":"opened","behavior":"failure","authorized":true},
		{"key":"other-v1","entity":"other","revision":"1","kind":"opened","behavior":"success","authorized":true},
		{"key":"none","entity":"none","revision":"1","kind":"ignored","behavior":"success","authorized":true},
		{"key":"denied","entity":"denied","revision":"1","kind":"opened","behavior":"success","authorized":false}]}`
	if err = os.WriteFile(fixturePath, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(selectionPath, []byte(`{"by_kind":{"opened":["stub"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal(err)
	}
	if err = Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal("rediscovery duplicated or failed:", err)
	}
	conn, err := connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	fake := &fakeJobs{jobs: map[string]kubeJob{}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	k := &kubeClient{client: server.Client(), host: server.URL, token: "fixture", namespace: "fullsend-runner", image: "test-image"}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var waiting, reserved int
	if err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM selected_runs WHERE state='waiting'),(SELECT count(*) FROM selected_runs WHERE state='reserved')`).Scan(&waiting, &reserved); err != nil {
		t.Fatal(err)
	}
	if waiting != 1 || reserved != 2 {
		t.Fatalf("expected conflict wait and cap 2; waiting=%d reserved=%d", waiting, reserved)
	}
	if extra, err := reserve(ctx, conn, 3, 2); err != nil || extra != nil {
		t.Fatalf("per-tenant cap failed: extra=%v err=%v", extra, err)
	}
	fake.finish("fs-poc-1-1", true)
	fake.finish("fs-poc-3-1", true)
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM selected_runs WHERE state='reserved'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 2 {
		t.Fatalf("terminal Jobs released reservations before cleanup was confirmed: %d", reserved)
	}
	fake.finish("fs-poc-2-1", false)
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var observations, succeeded, failed, noMatch, denied, cleaned int
	err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM observations),(SELECT count(*) FROM selected_runs WHERE state='succeeded'),(SELECT count(*) FROM selected_runs WHERE state='failed'),(SELECT count(*) FROM observations WHERE selection_state='no_match'),(SELECT count(*) FROM observations WHERE selection_state='denied'),(SELECT count(*) FROM attempts WHERE cleanup_state='done')`).Scan(&observations, &succeeded, &failed, &noMatch, &denied, &cleaned)
	if err != nil {
		t.Fatal(err)
	}
	if observations != 5 || succeeded != 2 || failed != 1 || noMatch != 1 || denied != 1 || cleaned != 3 {
		t.Fatal(fmt.Sprintf("unexpected durable counts: %d %d %d %d %d %d", observations, succeeded, failed, noMatch, denied, cleaned))
	}
	if err = Status(ctx); err != nil {
		t.Fatal(err)
	}
	if err = Retry(ctx, 0); err == nil {
		t.Fatal("accepted zero run ID")
	}
	if err = Retry(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	fake.finish("fs-poc-2-2", false)
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var retryAttempts, siblingAttempts int
	if err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM attempts WHERE run_id=2),(SELECT count(*) FROM attempts WHERE run_id=1)`).Scan(&retryAttempts, &siblingAttempts); err != nil {
		t.Fatal(err)
	}
	if retryAttempts != 2 || siblingAttempts != 1 {
		t.Fatalf("retry reran sibling or reused attempt: retry=%d sibling=%d", retryAttempts, siblingAttempts)
	}
	if err = Dismiss(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err = Retry(ctx, 2); err == nil {
		t.Fatal("retried dismissed run")
	}
	// A vanished active Job is an unknown outcome. It retains its reservation.
	lostFixture := `{"tenant":"test","source":"fixture","cursor":"c2","observations":[{"key":"lost","entity":"lost","revision":"1","kind":"opened","behavior":"active","authorized":true}]}`
	if err = os.WriteFile(fixturePath, []byte(lostFixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal(err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var lostName string
	if err = conn.QueryRow(ctx, `SELECT a.job_name FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id WHERE o.observation_key='lost'`).Scan(&lostName); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	delete(fake.jobs, lostName)
	fake.mu.Unlock()
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var unknown, stillReserved int
	if err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM attempts WHERE state='unknown'),(SELECT count(*) FROM selected_runs WHERE state='reserved')`).Scan(&unknown, &stillReserved); err != nil {
		t.Fatal(err)
	}
	if unknown != 1 || stillReserved != 1 {
		rows, err := conn.Query(ctx, `SELECT job_name,state FROM attempts ORDER BY id`)
		if err == nil {
			for rows.Next() {
				var name, state string
				_ = rows.Scan(&name, &state)
				t.Logf("attempt %s %s", name, state)
			}
			rows.Close()
		}
		t.Fatalf("vanished Job lost reservation: unknown=%d reserved=%d", unknown, stillReserved)
	}
	// A lost create response must retry the same attempt and discover the Job.
	ambiguousFixture := `{"tenant":"test","source":"fixture","cursor":"c3","observations":[{"key":"ambiguous","entity":"ambiguous","revision":"1","kind":"opened","behavior":"success","authorized":true}]}`
	if err = os.WriteFile(fixturePath, []byte(ambiguousFixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.lostCreate = true
	fake.mu.Unlock()
	if err = dispatchWithClient(ctx, conn, k); err == nil {
		t.Fatal("lost create response was reported as success")
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal("same-name create recovery failed:", err)
	}
	var ambiguousName string
	if err = conn.QueryRow(ctx, `SELECT a.job_name FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id WHERE o.observation_key='ambiguous'`).Scan(&ambiguousName); err != nil {
		t.Fatal(err)
	}
	fake.finish(ambiguousName, true)
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var attemptCount int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id WHERE o.observation_key='ambiguous'`).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 1 {
		t.Fatalf("ambiguous create made %d attempts", attemptCount)
	}
	cleanupFixture := `{"tenant":"test","source":"fixture","cursor":"c4","observations":[{"key":"cleanup","entity":"cleanup","revision":"1","kind":"opened","behavior":"success","authorized":true}]}`
	if err = os.WriteFile(fixturePath, []byte(cleanupFixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal(err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var cleanupName string
	if err = conn.QueryRow(ctx, `SELECT a.job_name FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id WHERE o.observation_key='cleanup'`).Scan(&cleanupName); err != nil {
		t.Fatal(err)
	}
	fake.finish(cleanupName, true)
	fake.mu.Lock()
	fake.deleteFailOnce = true
	fake.mu.Unlock()
	if err = dispatchWithClient(ctx, conn, k); err == nil {
		t.Fatal("delete failure was hidden")
	}
	var cleanupState string
	if err = conn.QueryRow(ctx, `SELECT a.state FROM attempts a WHERE a.job_name=$1`, cleanupName).Scan(&cleanupState); err != nil {
		t.Fatal(err)
	}
	if cleanupState != "cleanup" {
		t.Fatalf("outcome was not committed before delete: %s", cleanupState)
	}
	if err = conn.QueryRow(ctx, `SELECT r.state FROM selected_runs r JOIN attempts a ON a.run_id=r.id WHERE a.job_name=$1`, cleanupName).Scan(&cleanupState); err != nil {
		t.Fatal(err)
	}
	if cleanupState != "reserved" {
		t.Fatalf("delete failure released reservation: %s", cleanupState)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal("delete recovery failed:", err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal("cleanup confirmation failed:", err)
	}
	if err = conn.QueryRow(ctx, `SELECT a.state FROM attempts a WHERE a.job_name=$1`, cleanupName).Scan(&cleanupState); err != nil {
		t.Fatal(err)
	}
	if cleanupState != "complete" {
		t.Fatalf("cleanup did not finish: %s", cleanupState)
	}
	// Two selected harnesses are durable siblings. One can fail without
	// causing the successful sibling to run again.
	if err = os.WriteFile(selectionPath, []byte(`{"by_kind":{"opened":["alpha","beta"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	fanoutFixture := `{"tenant":"test","source":"fixture","cursor":"c5","observations":[{"key":"fanout","entity":"fanout","revision":"1","kind":"opened","behavior":"success","authorized":true}]}`
	if err = os.WriteFile(fixturePath, []byte(fanoutFixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Poll(ctx, fixturePath, selectionPath); err != nil {
		t.Fatal(err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var alphaName, betaName string
	if err = conn.QueryRow(ctx, `SELECT a.job_name FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id WHERE o.observation_key='fanout' AND r.harness='alpha'`).Scan(&alphaName); err != nil {
		t.Fatal(err)
	}
	fake.finish(alphaName, true)
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	// The unknown Job holds one slot. The successful Job releases its slot
	// only after the next cycle confirms Kubernetes deleted it.
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT a.job_name FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id WHERE o.observation_key='fanout' AND r.harness='beta'`).Scan(&betaName); err != nil {
		t.Fatal(err)
	}
	fake.finish(betaName, false)
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	if err = dispatchWithClient(ctx, conn, k); err != nil {
		t.Fatal(err)
	}
	var alphaState, betaState string
	if err = conn.QueryRow(ctx, `SELECT state FROM selected_runs WHERE harness='alpha'`).Scan(&alphaState); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT state FROM selected_runs WHERE harness='beta'`).Scan(&betaState); err != nil {
		t.Fatal(err)
	}
	if alphaState != "succeeded" || betaState != "failed" {
		t.Fatalf("partial fanout states: %s %s", alphaState, betaState)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM attempts WHERE job_name=$1`, alphaName).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 1 {
		t.Fatalf("successful sibling reran %d times", attemptCount)
	}
	// A stored, malformed selection revision is visible as an evaluation error.
	if _, err = conn.Exec(ctx, `INSERT INTO config_revisions(digest,content) VALUES('invalid-selection','{"by_kind":{"opened":"wrong-type"}}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO observations(tenant,source,observation_key,entity,revision,kind,behavior,authorized,config_digest) VALUES('test','fixture','invalid','invalid','1','opened','success',true,'invalid-selection')`); err != nil {
		t.Fatal(err)
	}
	if err = selectPending(ctx, conn); err != nil {
		t.Fatal(err)
	}
	var selectionState string
	if err = conn.QueryRow(ctx, `SELECT selection_state FROM observations WHERE observation_key='invalid'`).Scan(&selectionState); err != nil {
		t.Fatal(err)
	}
	if selectionState != "evaluation_error" {
		t.Fatalf("invalid selection state: %s", selectionState)
	}
	if err = k.delete(ctx, lostName, "uid-"+lostName); err != nil {
		t.Fatal("deleting already-missing Job should converge:", err)
	}
	// A missing Job with no terminal outcome cannot release a reservation.
	var invalidCleanupID int64
	if err = conn.QueryRow(ctx, `INSERT INTO attempts(run_id,number,job_name,state) VALUES(1,99,'fs-poc-invalid-cleanup','cleanup') RETURNING id`).Scan(&invalidCleanupID); err != nil {
		t.Fatal(err)
	}
	if err = reconcile(ctx, conn, k, attempt{id: invalidCleanupID, runID: 1, name: "fs-poc-invalid-cleanup", state: "cleanup"}); err == nil {
		t.Fatal("missing Job with no terminal outcome was marked complete")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if err = Dispatch(ctx); err == nil {
		t.Fatal("dispatch accepted missing Kubernetes identity")
	}
	if _, err = conn.Exec(ctx, `UPDATE schema_versions SET version=99`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx); err == nil {
		t.Fatal("migration accepted a newer schema")
	}
}
