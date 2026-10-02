package k8spoc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/jirapoll"
	"github.com/jackc/pgx/v5"
)

type liveFakeJira struct {
	jirapoll.JiraClient
	issues      []jira.Issue
	comments    map[string][]jira.Comment
	badComments string
	searchErr   error
	issueErr    map[string]error
	roles       map[string]bool
}

func (f *liveFakeJira) GetMyself(context.Context) (*jira.User, error) {
	return &jira.User{AccountID: "service", Active: true, AccountType: "atlassian"}, nil
}
func (f *liveFakeJira) SearchIssues(_ context.Context, jql string, limit int) ([]jira.Issue, error) {
	if !strings.Contains(jql, `project = "FSENDAI" AND labels = "fullsend-dispatcher-test"`) || limit != 0 {
		return nil, errors.New("unbounded Jira search")
	}
	return f.issues, f.searchErr
}
func (f *liveFakeJira) GetIssue(_ context.Context, key string) (*jira.Issue, error) {
	if err := f.issueErr[key]; err != nil {
		return nil, err
	}
	for i := range f.issues {
		if f.issues[i].Key == key {
			return &f.issues[i], nil
		}
	}
	return nil, nil
}
func (f *liveFakeJira) ListComments(_ context.Context, key string) ([]jira.Comment, error) {
	if key == f.badComments {
		return nil, errors.New("Jira page failed")
	}
	return f.comments[key], nil
}
func (f *liveFakeJira) ListChangelog(context.Context, string) ([]jira.ChangelogEntry, error) {
	return nil, nil
}
func (f *liveFakeJira) GetProjectRoleActors(context.Context, string) (map[string]jira.ProjectRoleActors, error) {
	return map[string]jira.ProjectRoleActors{"Developers": {DirectUsers: f.roles}}, nil
}

func liveDB(t *testing.T) (context.Context, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "poc_jira_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})
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
	conn, err := connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return ctx, conn
}

func TestLiveJiraDiscoverySelectionReplayAndRevision(t *testing.T) {
	ctx, conn := liveDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	stamp := func(offset time.Duration) string { return now.Add(offset).Format(time.RFC3339) }
	actor := jira.User{AccountID: "writer", Active: true, AccountType: "atlassian"}
	issue := func(id, key, who string) jira.Issue {
		u := actor
		u.AccountID = who
		return jira.Issue{ID: id, Key: key, Fields: jira.IssueFields{Created: stamp(-time.Minute), Updated: stamp(-time.Minute), Labels: []string{"fullsend-dispatcher-test"}, Reporter: u}}
	}
	fake := &liveFakeJira{issues: []jira.Issue{issue("1", "FSENDAI-1", "writer"), issue("2", "FSENDAI-2", "outsider")}, comments: map[string][]jira.Comment{}, roles: map[string]bool{"writer": true}}
	cfg := LiveConfig{JiraBaseURL: "https://jira.example", Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo", Harnesses: []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}}}
	poll := func() {
		t.Helper()
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err = pollJiraWithClient(ctx, conn, fake, cfg, raw, strconv.Itoa(len(raw))); err != nil {
			t.Fatal(err)
		}
	}
	selectAll := func() {
		t.Helper()
		candidates, err := pendingJira(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		if err = selectJiraCandidates(ctx, conn, candidates, func(baseURL string) (jirapoll.JiraClient, error) {
			if baseURL != cfg.JiraBaseURL {
				return nil, errors.New("unexpected URL")
			}
			return fake, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	counts := func(wantObs, wantRun, wantDenied int) {
		t.Helper()
		var obs, runs, denied int
		if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM observations),(SELECT count(*) FROM selected_runs),(SELECT count(*) FROM observations WHERE selection_state='denied')`).Scan(&obs, &runs, &denied); err != nil {
			t.Fatal(err)
		}
		if obs != wantObs || runs != wantRun || denied != wantDenied {
			t.Fatalf("observations=%d runs=%d denied=%d; want %d %d %d", obs, runs, denied, wantObs, wantRun, wantDenied)
		}
	}
	poll()
	t.Setenv("JIRA_TOKEN_FILE", "")
	t.Setenv("JIRA_EMAIL_FILE", "")
	if err := selectPendingJira(ctx, conn); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JIRA_TOKEN_FILE", filepath.Join(t.TempDir(), "token"))
	if err := selectPendingJira(ctx, conn); err == nil {
		t.Fatal("accepted incomplete Jira credentials")
	}
	t.Setenv("JIRA_TOKEN_FILE", "")
	selectAll()
	counts(2, 1, 1)
	poll()
	selectAll()
	counts(2, 1, 1)
	// A failed issue page must not prevent later eligible issues from being committed.
	fake.issues = append(fake.issues, issue("3", "FSENDAI-3", "writer"), issue("4", "FSENDAI-4", "writer"))
	fake.badComments = "FSENDAI-3"
	raw, _ := json.Marshal(cfg)
	if err := pollJiraWithClient(ctx, conn, fake, cfg, raw, strconv.Itoa(len(raw))); err == nil {
		t.Fatal("expected page error")
	}
	selectAll()
	counts(3, 2, 1)
	fake.badComments = ""
	poll()
	selectAll()
	counts(4, 3, 1)
	// A new comment is a distinct revision; changing the validated snapshot
	// affects that observation without changing prior selections.
	cfg.Harnesses[0].Trigger = `event.transition.kind == "comment_added"`
	fake.comments["FSENDAI-1"] = []jira.Comment{{ID: "9", Body: "/fs-triage", Author: actor, Created: stamp(time.Minute), Updated: stamp(time.Minute)}}
	poll()
	selectAll()
	counts(5, 4, 1)
	poll()
	selectAll()
	counts(5, 4, 1)
	var digests int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM config_revisions`).Scan(&digests); err != nil {
		t.Fatal(err)
	}
	if digests != 2 {
		t.Fatalf("want 2 immutable config revisions, got %d", digests)
	}
	// Failed search does not mutate durable discovery state.
	fake.searchErr = errors.New("search failed")
	if err := pollJiraWithClient(ctx, conn, fake, cfg, raw, "bad"); err == nil {
		t.Fatal("expected search error")
	}
	counts(5, 4, 1)
	fake.searchErr = nil
	// A refetch failure leaves its candidate pending while the next issue
	// is selected. Recovery processes the original candidate once Jira works.
	fake.issues = append(fake.issues, issue("5", "FSENDAI-5", "writer"), issue("6", "FSENDAI-6", "writer"))
	cfg.Harnesses[0].Trigger = `event.transition.kind == "opened"`
	poll()
	fake.issueErr = map[string]error{"FSENDAI-5": errors.New("refetch failed")}
	candidates, err := pendingJira(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := selectJiraCandidates(ctx, conn, candidates, func(string) (jirapoll.JiraClient, error) { return fake, nil }); err == nil {
		t.Fatal("expected refetch error")
	}
	counts(7, 5, 1)
	delete(fake.issueErr, "FSENDAI-5")
	selectAll()
	counts(7, 6, 1)
	// An opened event under a comment-only trigger is an explicit no-match.
	cfg.Harnesses[0].Trigger = `event.transition.kind == "comment_added"`
	fake.issues = append(fake.issues, issue("7", "FSENDAI-7", "writer"))
	poll()
	selectAll()
	var noMatch int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM observations WHERE entity='FSENDAI-7' AND selection_state='no_match'`).Scan(&noMatch); err != nil || noMatch != 1 {
		t.Fatalf("no-match state %d: %v", noMatch, err)
	}
	// A corrupt persisted snapshot is observable and cannot turn into a no-match.
	if _, err := conn.Exec(ctx, `INSERT INTO config_revisions(digest,content) VALUES('invalid','"bad"')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO observations(tenant,source,observation_key,entity,revision,kind,behavior,authorized,config_digest) VALUES('owner/repo','jira:FSENDAI:fullsend-dispatcher-test','poison','FSENDAI-7','bad','opened','success',false,'invalid')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO config_revisions(digest,content) VALUES('incomplete','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO observations(tenant,source,observation_key,entity,revision,kind,behavior,authorized,config_digest) VALUES('owner/repo','jira:FSENDAI:fullsend-dispatcher-test','poison-2','FSENDAI-7','bad','opened','success',false,'incomplete')`); err != nil {
		t.Fatal(err)
	}
	candidates, err = pendingJira(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if err = selectJiraCandidates(ctx, conn, candidates, func(string) (jirapoll.JiraClient, error) { return fake, nil }); err != nil {
		t.Fatal(err)
	}
	var evalError int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM observations WHERE observation_key LIKE 'poison%' AND selection_state='evaluation_error'`).Scan(&evalError); err != nil || evalError != 2 {
		t.Fatalf("poison state %d: %v", evalError, err)
	}
}

func TestLiveConfigValidationAndEntrypoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.json")
	cfg := LiveConfig{JiraBaseURL: "https://jira.example", Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo", Harnesses: []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, gotRaw, digest, err := readLiveConfig(path)
	if err != nil || got.Repo != cfg.Repo || string(gotRaw) != string(raw) || len(digest) != 64 {
		t.Fatalf("read config: %v", err)
	}
	if err = PollJira(context.Background(), path, filepath.Join(dir, "missing-token"), filepath.Join(dir, "missing-email")); err == nil {
		t.Fatal("accepted missing credentials")
	}
	if _, _, _, err = readLiveConfig(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("accepted missing config")
	}
	for _, bad := range []LiveConfig{
		{JiraBaseURL: "http://jira.example", Project: cfg.Project, Label: cfg.Label, Repo: cfg.Repo, Harnesses: cfg.Harnesses},
		{JiraBaseURL: cfg.JiraBaseURL, Project: "bad key", Label: cfg.Label, Repo: cfg.Repo, Harnesses: cfg.Harnesses},
		{JiraBaseURL: cfg.JiraBaseURL, Project: cfg.Project, Label: cfg.Label, Repo: cfg.Repo},
		{JiraBaseURL: cfg.JiraBaseURL, Project: cfg.Project, Label: cfg.Label, Repo: cfg.Repo, Harnesses: []LiveHarness{{Name: "triage", Role: "triage", Trigger: "not CEL !!!"}}},
	} {
		if err = bad.validate(); err == nil {
			t.Fatalf("accepted invalid config: %+v", bad)
		}
	}
	if err = os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = readLiveConfig(path); err == nil {
		t.Fatal("accepted invalid JSON")
	}
}

func TestPrepareLiveConfig(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	configDir := filepath.Join(repoDir, ".fullsend")
	if err := os.MkdirAll(filepath.Join(configDir, "harness"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "repos.yaml")
	if err := os.WriteFile(manifest, []byte("version: 1\ngithub:\n  repos:\n    - name: owner/repo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("version: \"1\"\nagents:\n  - name: triage\n    source: harness/triage.yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "harness", "triage.yaml"), []byte("agent: triage\nrole: triage\ntrigger: 'event.transition.kind == \"opened\"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "live.json")
	call := func(slug string) error {
		return PrepareLiveConfig(context.Background(), manifest, repoDir, slug, "https://jira.example", "FSENDAI", "fullsend-dispatcher-test", output)
	}
	if err := PrepareLiveConfig(context.Background(), filepath.Join(dir, "missing"), repoDir, "owner/repo", "https://jira.example", "FSENDAI", "fullsend-dispatcher-test", output); err == nil {
		t.Fatal("accepted missing manifest")
	}
	if err := call("invalid"); err == nil {
		t.Fatal("accepted invalid slug")
	}
	if err := call("other/repo"); err == nil {
		t.Fatal("accepted unregistered repo")
	}
	if err := call("owner/repo"); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err := readLiveConfig(output)
	if err != nil || cfg.Repo != "owner/repo" || len(cfg.Harnesses) != 1 || cfg.Harnesses[0].Name != "triage" {
		t.Fatalf("snapshot: %+v, %v", cfg, err)
	}
	if err := PrepareLiveConfig(context.Background(), manifest, repoDir, "owner/repo", "http://not-loopback.example", "FSENDAI", "fullsend-dispatcher-test", output); err == nil {
		t.Fatal("accepted insecure Jira URL")
	}
	broken := "version: \"1\"\nagents:\n  - name: triage\n    source: harness/triage.yaml\n  - name: broken\n    source: harness/missing.yaml\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	if err := call("owner/repo"); err == nil {
		t.Fatal("activated partial harness revision")
	}
}

func TestLivePollRejectsOutOfScopeAndBadCheckpoint(t *testing.T) {
	ctx, conn := liveDB(t)
	cfg := LiveConfig{JiraBaseURL: "https://jira.example", Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo", Harnesses: []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}}}
	raw, _ := json.Marshal(cfg)
	issue := jira.Issue{ID: "1", Key: "WRONG-1", Fields: jira.IssueFields{Created: time.Now().UTC().Format(time.RFC3339), Labels: []string{cfg.Label}}}
	fake := &liveFakeJira{issues: []jira.Issue{issue}}
	if err := pollJiraWithClient(ctx, conn, fake, cfg, raw, "digest"); err == nil {
		t.Fatal("accepted issue outside project")
	}
	fake.issues[0].Key = "FSENDAI-1"
	fake.issues[0].Fields.Labels = nil
	if err := pollJiraWithClient(ctx, conn, fake, cfg, raw, "digest"); err == nil {
		t.Fatal("accepted issue without scope label")
	}
	fake.issues[0].Fields.Labels = []string{cfg.Label}
	if _, err := conn.Exec(ctx, `INSERT INTO source_checkpoints(tenant,source,cursor) VALUES($1,$2,'invalid')`, cfg.Repo, "jira:FSENDAI:fullsend-dispatcher-test:1"); err != nil {
		t.Fatal(err)
	}
	if err := pollJiraWithClient(ctx, conn, fake, cfg, raw, "digest"); err == nil {
		t.Fatal("accepted corrupt checkpoint")
	}
	var observations int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM observations`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("unexpected observations %d: %v", observations, err)
	}
}

func TestLiveSelectionRejectsScopeLoss(t *testing.T) {
	ctx, conn := liveDB(t)
	cfg := LiveConfig{JiraBaseURL: "https://jira.example", Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo", Harnesses: []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}}}
	raw, _ := json.Marshal(cfg)
	fake := &liveFakeJira{issues: []jira.Issue{{ID: "1", Key: "FSENDAI-1", Fields: jira.IssueFields{Created: time.Now().UTC().Format(time.RFC3339), Labels: []string{cfg.Label}, Reporter: jira.User{AccountID: "writer", Active: true}}}}}
	if err := pollJiraWithClient(ctx, conn, fake, cfg, raw, "digest"); err != nil {
		t.Fatal(err)
	}
	candidates, err := pendingJira(ctx, conn)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("pending candidates %d: %v", len(candidates), err)
	}
	fake.issues[0].Fields.Labels = nil
	if err := selectJiraCandidate(ctx, conn, fake, candidates[0]); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := conn.QueryRow(ctx, `SELECT selection_state FROM observations WHERE id=$1`, candidates[0].id).Scan(&state); err != nil || state != "denied" {
		t.Fatalf("scope loss state %q: %v", state, err)
	}
}

func TestPollJiraHTTP(t *testing.T) {
	ctx, conn := liveDB(t)
	created := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	var serverURL string
	issue := jira.Issue{ID: "8", Key: "FSENDAI-8", Fields: jira.IssueFields{Created: created, Updated: created, Labels: []string{"fullsend-dispatcher-test"}, Reporter: jira.User{AccountID: "writer", Active: true}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, _, ok := r.BasicAuth(); !ok {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/rest/api/3/myself":
			_, _ = w.Write([]byte(`{"accountId":"service","active":true}`))
		case r.URL.Path == "/rest/api/3/search/jql":
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": []jira.Issue{issue}, "isLast": true})
		case r.URL.Path == "/rest/api/3/issue/FSENDAI-8":
			_ = json.NewEncoder(w).Encode(issue)
		case r.URL.Path == "/rest/api/3/project/FSENDAI/role":
			_ = json.NewEncoder(w).Encode(map[string]string{"Developers": serverURL + "/rest/api/3/project/FSENDAI/role/1"})
		case r.URL.Path == "/rest/api/3/project/FSENDAI/role/1":
			_ = json.NewEncoder(w).Encode(jira.ProjectRoleDetail{Name: "Developers", Actors: []jira.RoleActor{{ActorUser: &jira.RoleActorUser{AccountID: "writer"}}}})
		case strings.HasSuffix(r.URL.Path, "/comment"):
			_, _ = w.Write([]byte(`{"comments":[],"total":0}`))
		case strings.HasSuffix(r.URL.Path, "/changelog"):
			_, _ = w.Write([]byte(`{"values":[],"isLast":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL
	cfg := LiveConfig{JiraBaseURL: server.URL, Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo", Harnesses: []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}}}
	dir := t.TempDir()
	configFile, tokenFile, emailFile := filepath.Join(dir, "live.json"), filepath.Join(dir, "token"), filepath.Join(dir, "email")
	raw, _ := json.Marshal(cfg)
	for path, body := range map[string][]byte{configFile: raw, tokenFile: []byte("secret"), emailFile: []byte("test@example.com")} {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := PollJira(ctx, configFile, tokenFile, emailFile); err != nil {
		t.Fatal(err)
	}
	if err := PollJira(ctx, configFile, tokenFile, emailFile); err != nil {
		t.Fatal(err)
	}
	var observed int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM observations WHERE entity='FSENDAI-8'`).Scan(&observed); err != nil || observed != 1 {
		t.Fatalf("observations %d: %v", observed, err)
	}
	t.Setenv("JIRA_TOKEN_FILE", tokenFile)
	t.Setenv("JIRA_EMAIL_FILE", emailFile)
	if err := selectPendingJira(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := selectPendingJira(ctx, conn); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM selected_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("selected runs %d: %v", runs, err)
	}
}
