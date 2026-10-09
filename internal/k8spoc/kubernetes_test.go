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
