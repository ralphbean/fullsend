package k8spoc

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubernetesJobContract(t *testing.T) {
	var posted map[string]any
	var deleted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing authorization")
		}
		switch r.Method {
		case "GET":
			http.NotFound(w, r)
		case "POST":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"name":"fs-poc-1-1","uid":"uid-1"}}`))
		case "DELETE":
			if err := json.NewDecoder(r.Body).Decode(&deleted); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	k := &kubeClient{client: server.Client(), host: server.URL, token: "test-token", namespace: "fullsend-runner", image: "busybox:1.36.1"}
	ctx := context.Background()
	if _, found, err := k.get(ctx, "fs-poc-1-1"); err != nil || found {
		t.Fatalf("missing Job: found=%v err=%v", found, err)
	}
	job, err := k.create(ctx, attempt{id: 1, name: "fs-poc-1-1", behavior: "failure"})
	if err != nil {
		t.Fatal(err)
	}
	if job.Metadata.UID != "uid-1" {
		t.Fatal(job)
	}
	spec := posted["spec"].(map[string]any)
	if spec["backoffLimit"] != float64(0) {
		t.Fatal("unexpected retry policy")
	}
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false {
		t.Fatal("runner has API token")
	}
	if err := k.delete(ctx, "fs-poc-1-1", job.Metadata.UID); err != nil {
		t.Fatal(err)
	}
	pre := deleted["preconditions"].(map[string]any)
	if pre["uid"] != "uid-1" {
		t.Fatal("delete lacks UID precondition")
	}
	if deleted["propagationPolicy"] != "Foreground" {
		t.Fatal("delete did not request foreground cleanup")
	}
}

func TestConfigureMintRepositoryID(t *testing.T) {
	k := &kubeClient{}
	if err := configureMintRepositoryID(k, ""); err != nil || k.mintRepositoryID != 0 {
		t.Fatalf("empty repository ID should leave mint disabled: id=%d err=%v", k.mintRepositoryID, err)
	}
	for _, raw := range []string{"0", "-1", "+123", "0123", "not-an-id"} {
		if err := configureMintRepositoryID(k, raw); err == nil {
			t.Errorf("accepted non-canonical repository ID %q", raw)
		}
	}
	if err := configureMintRepositoryID(k, "123456789"); err != nil || k.mintRepositoryID != 123456789 {
		t.Fatalf("valid repository ID not configured: id=%d err=%v", k.mintRepositoryID, err)
	}
}

func TestEnsureJiraEventConfigMapLifecycle(t *testing.T) {
	event := []byte(`{"source":{"system":"jira"}}`)
	var posted map[string]any
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			if deleted {
				http.NotFound(w, r)
				return
			}
			deleted = true
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	k := &kubeClient{client: server.Client(), host: server.URL, namespace: "fullsend-runner"}
	a := attempt{id: 1, runID: 42, eventPayload: event}
	if err := k.ensureJiraEventConfigMap(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	data := posted["data"].(map[string]any)
	if posted["kind"] != "ConfigMap" || posted["immutable"] != true || posted["metadata"].(map[string]any)["name"] != "fullsend-event-42" || data["event.json"] != string(event) {
		t.Fatalf("unexpected event ConfigMap: %v", posted)
	}
	if err := k.deleteJiraEventConfigMap(context.Background(), a.runID); err != nil || !deleted {
		t.Fatalf("event ConfigMap deletion: deleted=%v err=%v", deleted, err)
	}
	if err := k.ensureJiraEventConfigMap(context.Background(), attempt{id: 2, runID: 42, eventPayload: []byte("not-json")}); err == nil {
		t.Fatal("accepted invalid event JSON")
	}
	missing := &kubeClient{client: server.Client(), host: server.URL, namespace: "fullsend-runner"}
	if err := missing.deleteJiraEventConfigMap(context.Background(), 42); err != nil {
		t.Fatal("deleting an already absent event ConfigMap should be idempotent:", err)
	}
}

func TestEnsureJiraEventConfigMapConflicts(t *testing.T) {
	event := `{"source":{"system":"jira"}}`
	for _, tc := range []struct {
		name    string
		stored  string
		wantErr bool
	}{{name: "same event is idempotent", stored: event}, {name: "conflicting event is rejected", stored: `{"source":{"system":"other"}}`, wantErr: true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusConflict)
					return
				}
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"name": "fullsend-event-42"}, "data": map[string]string{"event.json": tc.stored}})
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			k := &kubeClient{client: server.Client(), host: server.URL, namespace: "fullsend-runner"}
			err := k.ensureJiraEventConfigMap(context.Background(), attempt{runID: 42, eventPayload: []byte(event)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("ensureJiraEventConfigMap error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestKubernetesCreateRealJiraTriageJob(t *testing.T) {
	configBytes, err := json.Marshal(LiveConfig{
		JiraBaseURL: "https://stage.example", Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo",
		Harnesses:         []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}},
		FullsendHarnesses: []string{"triage"}, RepoBundle: "fullsend-live-repo-abcdef0123456789", RepoRevision: strings.Repeat("a", 40),
		GCPAudience: "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/pool/providers/provider", VertexProject: "vertex-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	a := attempt{id: 2, runID: 42, name: "fs-poc-42-1", harness: "triage", execution: "fullsend", source: "jira:FSENDAI:fullsend-dispatcher-test", eventPayload: []byte(validTestJiraEventPayload), configContent: configBytes}
	var postedConfig, postedJob map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/fullsend-runner/serviceaccounts/fs-repo-123456789-triage" && r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/v1/namespaces/fullsend-runner/serviceaccounts" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.URL.Path == "/api/v1/namespaces/fullsend-runner/configmaps" {
			if err := json.NewDecoder(r.Body).Decode(&postedConfig); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.URL.Path == "/apis/batch/v1/namespaces/fullsend-runner/jobs" {
			if err := json.NewDecoder(r.Body).Decode(&postedJob); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"name":"fs-poc-42-1","uid":"uid"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	k := &kubeClient{client: server.Client(), host: server.URL, namespace: "fullsend-runner", image: "busybox:1.36.1", gatewayImage: "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef", mintRepositoryID: 123456789}
	if _, err = k.create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if postedConfig == nil || postedJob == nil {
		t.Fatal("expected event ConfigMap and real runner Job")
	}
	pod := postedJob["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["serviceAccountName"] != "fs-repo-123456789-triage" {
		t.Fatalf("runner service account = %v", pod["serviceAccountName"])
	}
	if !strings.Contains(string(mustJSON(t, postedJob)), "fullsend-event-42") || !strings.Contains(string(mustJSON(t, postedJob)), "JIRA_TOKEN") {
		t.Fatal("real Job is missing its durable event or host-side Jira credentials")
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	for _, raw := range container["env"].([]any) {
		entry := raw.(map[string]any)
		if entry["name"] == "FULLSEND_WORK_ITEM_URL" && entry["value"] == "https://stage.example/browse/FSENDAI-123" {
			return
		}
	}
	t.Fatal("real Jira Job is missing the selected issue URL")
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnsureMintServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		conflict              bool
		conflictWithoutWinner bool
		getForbidden          bool
		postForbidden         bool
		preexisting           *kubeServiceAccount
		wantErr               bool
		wantPostCount         int
	}{
		{name: "create", wantPostCount: 1},
		{name: "concurrent create conflict with matching winner", conflict: true, wantPostCount: 1},
		{name: "get forbidden", getForbidden: true, wantErr: true},
		{name: "create forbidden", postForbidden: true, wantErr: true, wantPostCount: 1},
		{name: "conflict without winner", conflict: true, conflictWithoutWinner: true, wantErr: true, wantPostCount: 1},
		{name: "existing matching account", preexisting: mintServiceAccountFixture("fs-repo-123456789-triage", "123456789", gatewayDemoRole), wantPostCount: 0},
		{name: "existing conflicting account", preexisting: mintServiceAccountFixture("fs-repo-123456789-triage", "123456789", "coder"), wantErr: true, wantPostCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var account *kubeServiceAccount
			if tc.preexisting != nil {
				account = tc.preexisting
			}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					if tc.getForbidden {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					if account == nil {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(account)
				case http.MethodPost:
					posts++
					var created kubeServiceAccount
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					b, _ := json.Marshal(body)
					if err := json.Unmarshal(b, &created); err != nil {
						t.Error(err)
						return
					}
					if created.Metadata.Labels["fullsend.ai/repository-id"] != "123456789" ||
						created.Metadata.Labels["fullsend.ai/role"] != gatewayDemoRole ||
						created.AutomountServiceAccountToken == nil || *created.AutomountServiceAccountToken {
						t.Errorf("unsafe service account request: %+v", created)
					}
					if tc.postForbidden {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					if tc.conflict {
						if !tc.conflictWithoutWinner {
							account = mintServiceAccountFixture("fs-repo-123456789-triage", "123456789", gatewayDemoRole)
						}
						w.WriteHeader(http.StatusConflict)
						return
					}
					account = &created
					w.WriteHeader(http.StatusCreated)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusMethodNotAllowed)
				}
			}))
			defer server.Close()
			k := &kubeClient{client: server.Client(), host: server.URL, namespace: "fullsend-runner"}
			err := k.ensureMintServiceAccount(context.Background(), 123456789, gatewayDemoRole)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ensureMintServiceAccount error = %v, wantErr %v", err, tc.wantErr)
			}
			if posts != tc.wantPostCount {
				t.Fatalf("service account POST count = %d, want %d", posts, tc.wantPostCount)
			}
		})
	}
	if err := (&kubeClient{}).ensureMintServiceAccount(context.Background(), 0, gatewayDemoRole); err == nil {
		t.Fatal("accepted empty repository identity")
	}
	if err := (&kubeClient{}).ensureMintServiceAccount(context.Background(), 123456789, "coder"); err == nil {
		t.Fatal("accepted a role outside the dispatcher demo allowlist")
	}
}

func TestCreateGatewayAttemptUsesMintIdentity(t *testing.T) {
	var postedJob map[string]any
	var postedServiceAccount map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/serviceaccounts/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/serviceaccounts"):
			if err := json.NewDecoder(r.Body).Decode(&postedServiceAccount); err != nil {
				t.Error(err)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			if err := json.NewDecoder(r.Body).Decode(&postedJob); err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"name":"fs-poc-23-2","uid":"uid-mint"}}`))
		default:
			t.Errorf("unexpected Kubernetes request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	k := &kubeClient{
		client: server.Client(), host: server.URL, namespace: "fullsend-runner",
		gatewayImage:     "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef",
		mintRepositoryID: 123456789,
	}
	job, err := k.create(context.Background(), attempt{
		id: 17, name: "fs-poc-23-2", harness: "gateway-demo", source: "fixture-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Metadata.UID != "uid-mint" || postedServiceAccount == nil || postedJob == nil {
		t.Fatalf("mint launch incomplete: job=%+v serviceAccount=%v postedJob=%v", job, postedServiceAccount, postedJob)
	}
	pod := postedJob["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["serviceAccountName"] != "fs-repo-123456789-triage" {
		t.Fatalf("dispatcher Job used service account %v", pod["serviceAccountName"])
	}
	if postedServiceAccount["kind"] != "ServiceAccount" {
		t.Fatalf("launcher did not create a service account: %v", postedServiceAccount)
	}
}

func mintServiceAccountFixture(name, repositoryID, role string) *kubeServiceAccount {
	disabled := false
	return &kubeServiceAccount{
		Metadata: struct {
			Name        string            `json:"name"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		}{
			Name: name,
			Labels: map[string]string{
				"fullsend.ai/repository-id": repositoryID,
				"fullsend.ai/role":          role,
			},
			Annotations: map[string]string{"fullsend.ai/mint-identity": "true"},
		},
		AutomountServiceAccountToken: &disabled,
	}
}

func TestKubernetesClientTLSAndCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("token was not sent")
		}
		_, _ = w.Write([]byte(`{"metadata":{"name":"job","uid":"uid"}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	caPath := filepath.Join(dir, "ca")
	if err := os.WriteFile(tokenPath, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, cert, 0600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	k, err := newKubeClientWithFiles(host, port, "test-image", tokenPath, caPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := k.get(context.Background(), "job"); err != nil || !found {
		t.Fatalf("TLS API call failed: found=%v err=%v", found, err)
	}
	if _, err := newKubeClientWithFiles(host, port, "", tokenPath, caPath); err == nil {
		t.Fatal("accepted empty image")
	}
	if _, err := newKubeClientWithFiles(host, port, "test", filepath.Join(dir, "missing"), caPath); err == nil {
		t.Fatal("accepted missing token")
	}
	if err := os.WriteFile(caPath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newKubeClientWithFiles(host, port, "test", tokenPath, caPath); err == nil {
		t.Fatal("accepted invalid CA")
	}
	if _, err := newKubeClientWithFiles(host, port, "test", tokenPath, filepath.Join(dir, "missing-ca")); err == nil {
		t.Fatal("accepted missing CA")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := newKubeClient(); err == nil {
		t.Fatal("accepted missing Kubernetes host")
	}
}

func TestGatewayAttemptCreationRequiresPinnedRunnerImage(t *testing.T) {
	k := &kubeClient{image: "stub-image"}
	a := attempt{id: 4, name: "fs-poc-7-1", harness: "gateway-demo", source: "fixture-source"}
	if _, err := k.create(context.Background(), a); err == nil {
		t.Fatal("gateway attempt silently fell back to a stub without a pinned runner image")
	}
	if _, err := k.createGatewayCleanup(context.Background(), a); err == nil {
		t.Fatal("gateway cleanup accepted an unpinned runner image")
	}
}
