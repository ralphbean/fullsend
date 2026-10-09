package mintclient

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fullsend-ai/fullsend/internal/mintcore/mintconsts"
)

func init() {
	retryBaseDelay = 0
}

func TestMintToken_HappyPath(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "bearer test-request-token" {
			t.Errorf("OIDC auth header = %q, want %q", got, "bearer test-request-token")
		}
		if got := r.URL.Query().Get("audience"); got != "fullsend-mint" {
			t.Errorf("audience = %q, want %q", got, "fullsend-mint")
		}
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "oidc-jwt-value"})
	}))
	defer oidcServer.Close()

	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/token" {
			t.Errorf("path = %q, want /v1/token", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oidc-jwt-value" {
			t.Errorf("mint auth header = %q, want %q", got, "Bearer oidc-jwt-value")
		}

		var body mintRequestBody
		json.NewDecoder(r.Body).Decode(&body)
		if body.Role != "triage" {
			t.Errorf("role = %q, want %q", body.Role, "triage")
		}
		if len(body.Repos) != 1 || body.Repos[0] != "my-repo" {
			t.Errorf("repos = %v, want [my-repo]", body.Repos)
		}

		json.NewEncoder(w).Encode(MintResult{
			Token:     "ghu_test_token",
			ExpiresAt: "2026-06-11T23:30:00Z",
		})
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?dummy=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "test-request-token"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	result, err := MintToken(context.Background(), MintRequest{
		MintURL: mintServer.URL,
		Role:    "triage",
		Repos:   []string{"my-repo"},
	})
	if err != nil {
		t.Fatalf("MintToken() error = %v", err)
	}
	if result.Token != "ghu_test_token" {
		t.Errorf("token = %q, want %q", result.Token, "ghu_test_token")
	}
	if result.ExpiresAt != "2026-06-11T23:30:00Z" {
		t.Errorf("expires_at = %q, want %q", result.ExpiresAt, "2026-06-11T23:30:00Z")
	}
}

func TestMintToken_KubernetesProjectionIsRereadForRetry(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first.header.signature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var authHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/kubernetes/token" {
			t.Errorf("path = %q, want Kubernetes identity endpoint", r.URL.Path)
		}
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		var body kubernetesMintRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		if body.RepositoryID != 123456 || body.Role != "coder" || body.Level != "write" {
			t.Errorf("request scope = %+v", body)
		}
		if len(authHeaders) == 1 {
			if err := os.WriteFile(tokenFile, []byte("rotated.header.signature\n"), 0600); err != nil {
				t.Errorf("rotating projected token: %v", err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(MintResult{Token: "ghs_kubernetes", ExpiresAt: "2099-01-01T00:00:00Z"})
	}))
	defer server.Close()

	result, err := MintToken(context.Background(), MintRequest{
		MintURL: server.URL, Role: "coder", Level: "write", RepositoryID: 123456, KubernetesTokenFile: tokenFile,
	})
	if err != nil {
		t.Fatalf("MintToken() error = %v", err)
	}
	if result.Token != "ghs_kubernetes" {
		t.Fatalf("token = %q, want ghs_kubernetes", result.Token)
	}
	if len(authHeaders) != 2 || authHeaders[0] != "Bearer first.header.signature" || authHeaders[1] != "Bearer rotated.header.signature" {
		t.Fatalf("Authorization headers = %v, want the initial and rotated projections", authHeaders)
	}
}

func TestMintToken_KubernetesRefreshDenialFailsClosed(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected.header.signature"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"refresh denied"}`))
	}))
	defer server.Close()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: server.URL, Role: "coder", Level: "write", RepositoryID: 123456, KubernetesTokenFile: tokenFile,
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("MintToken() error = %v, want 403 denial", err)
	}
	if calls != 1 {
		t.Fatalf("mint calls = %d, want one non-retryable authorization denial", calls)
	}
}

func TestValidMintURL(t *testing.T) {
	tests := []struct {
		name               string
		value              string
		kubernetesIdentity bool
		want               bool
	}{
		{name: "public HTTPS", value: "https://mint.example/v1/token", want: true},
		{name: "localhost test server", value: "http://localhost:8080", want: true},
		{name: "Kubernetes service over TLS", value: "https://fullsend-mint.fullsend-mint.svc", kubernetesIdentity: true, want: true},
		{name: "Kubernetes service over HTTP", value: "http://fullsend-mint.fullsend-mint.svc:8080", kubernetesIdentity: true, want: false},
		{name: "external HTTP with projected identity", value: "http://mint.example", kubernetesIdentity: true, want: false},
		{name: "URL credentials", value: "https://user:pass@mint.example", want: false},
		{name: "unsupported scheme", value: "ftp://mint.example", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := url.Parse(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := validMintURL(parsed, tt.kubernetesIdentity); got != tt.want {
				t.Fatalf("validMintURL() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestKubernetesMintHTTPClientTrustsConfiguredCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	caPath := filepath.Join(t.TempDir(), "mint-ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_MINT_CA_FILE", caPath)
	client, err := kubernetesMintHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("mint client did not trust configured CA: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestKubernetesMintHTTPClientRejectsInvalidCAFiles(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		t.Setenv("KUBERNETES_MINT_CA_FILE", filepath.Join(t.TempDir(), "missing.pem"))
		if _, err := kubernetesMintHTTPClient(); err == nil || !strings.Contains(err.Error(), "reading Kubernetes mint CA") {
			t.Fatalf("error = %v, want CA read failure", err)
		}
	})
	t.Run("no certificates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.pem")
		if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUBERNETES_MINT_CA_FILE", path)
		if _, err := kubernetesMintHTTPClient(); err == nil || !strings.Contains(err.Error(), "contains no certificates") {
			t.Fatalf("error = %v, want invalid CA failure", err)
		}
	})
}

type mintHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f mintHTTPDoerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

type mintErrorReader struct{}

func (mintErrorReader) Read([]byte) (int, error) { return 0, fmt.Errorf("read failed") }
func (mintErrorReader) Close() error             { return nil }

func TestCallKubernetesMintRejectsInvalidInputsAndResponses(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("header.payload.signature"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("repository ID missing", func(t *testing.T) {
		t.Setenv("FULLSEND_REPOSITORY_ID", "")
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example", Role: "coder", Level: "write"}, tokenFile)
		if err == nil || !strings.Contains(err.Error(), "FULLSEND_REPOSITORY_ID") {
			t.Fatalf("error = %v, want missing repository ID", err)
		}
	})
	t.Run("repository ID invalid", func(t *testing.T) {
		t.Setenv("FULLSEND_REPOSITORY_ID", "bad")
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example", Role: "coder", Level: "write"}, tokenFile)
		if err == nil {
			t.Fatal("invalid repository ID was accepted")
		}
	})
	t.Run("repository ID nonpositive", func(t *testing.T) {
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example", Role: "coder", Level: "write", RepositoryID: -1}, tokenFile)
		if err == nil || !strings.Contains(err.Error(), "positive") {
			t.Fatalf("error = %v, want nonpositive repository ID", err)
		}
	})
	t.Run("missing role", func(t *testing.T) {
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example", Level: "write", RepositoryID: 1}, tokenFile)
		if err == nil || !strings.Contains(err.Error(), "role is required") {
			t.Fatalf("error = %v, want missing role", err)
		}
	})
	t.Run("missing level", func(t *testing.T) {
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example", Role: "coder", RepositoryID: 1}, tokenFile)
		if err == nil || !strings.Contains(err.Error(), "level is required") {
			t.Fatalf("error = %v, want missing level", err)
		}
	})
	t.Run("invalid URL path", func(t *testing.T) {
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example/%zz", Role: "coder", Level: "write", RepositoryID: 1}, tokenFile)
		if err == nil || !strings.Contains(err.Error(), "constructing Kubernetes mint URL") {
			t.Fatalf("error = %v, want invalid URL path", err)
		}
	})
	t.Run("missing projected file", func(t *testing.T) {
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example", Role: "coder", Level: "write", RepositoryID: 1}, filepath.Join(t.TempDir(), "missing"))
		if err == nil || !strings.Contains(err.Error(), "reading projected Kubernetes mint token") {
			t.Fatalf("error = %v, want projected token read failure", err)
		}
	})
	t.Run("malformed projected JWT", func(t *testing.T) {
		badFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(badFile, []byte("not-a-jwt"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := callKubernetesMint(context.Background(), MintRequest{MintURL: "https://mint.example", Role: "coder", Level: "write", RepositoryID: 1}, badFile)
		if err == nil || !strings.Contains(err.Error(), "not a compact JWT") {
			t.Fatalf("error = %v, want invalid projected JWT", err)
		}
	})
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{name: "server failure", code: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`},
		{name: "invalid JSON response", code: http.StatusOK, body: `{`},
		{name: "empty token response", code: http.StatusOK, body: `{"token":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := MintToken(context.Background(), MintRequest{MintURL: server.URL, Role: "coder", Level: "write", RepositoryID: 1, KubernetesTokenFile: tokenFile})
			if err == nil {
				t.Fatal("invalid mint response was accepted")
			}
			if tc.code >= 500 && calls != 5 {
				t.Fatalf("retryable server response calls = %d, want 5", calls)
			}
			if tc.code < 500 && calls != 1 {
				t.Fatalf("non-retryable response calls = %d, want 1", calls)
			}
		})
	}
	t.Run("response body read failure", func(t *testing.T) {
		oldClient := httpClient
		defer func() { httpClient = oldClient }()
		httpClient = mintHTTPDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: mintErrorReader{}}, nil
		})
		_, err := MintToken(context.Background(), MintRequest{MintURL: "https://mint.example", Role: "coder", Level: "write", RepositoryID: 1, KubernetesTokenFile: tokenFile})
		if err == nil || !strings.Contains(err.Error(), "reading Kubernetes mint response") {
			t.Fatalf("error = %v, want response read failure", err)
		}
	})
}

func TestMintToken_CrossOrgTarget(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "oidc-jwt-value"})
	}))
	defer oidcServer.Close()

	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body mintRequestBody
		json.NewDecoder(r.Body).Decode(&body)
		if body.Role != "e2e" {
			t.Errorf("role = %q, want %q", body.Role, "e2e")
		}
		if body.TargetOrg != "halfsend-01" {
			t.Errorf("target_org = %q, want %q", body.TargetOrg, "halfsend-01")
		}
		if len(body.Repos) != 1 || body.Repos[0] != "*" {
			t.Errorf("repos = %v, want [\"*\"] for installation-wide e2e token", body.Repos)
		}

		json.NewEncoder(w).Encode(MintResult{
			Token:     "ghu_e2e_token",
			ExpiresAt: "2026-06-11T23:30:00Z",
		})
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?dummy=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "test-request-token"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	result, err := MintToken(context.Background(), MintRequest{
		MintURL:   mintServer.URL,
		Role:      "e2e",
		Repos:     []string{"*"},
		TargetOrg: "halfsend-01",
	})
	if err != nil {
		t.Fatalf("MintToken() error = %v", err)
	}
	if result.Token != "ghu_e2e_token" {
		t.Errorf("token = %q, want %q", result.Token, "ghu_e2e_token")
	}
}

func TestMintToken_OmittedRepos_Rejected(t *testing.T) {
	_, err := MintToken(context.Background(), MintRequest{
		MintURL: "https://mint.example.com",
		Role:    "triage",
	})
	if err == nil {
		t.Fatal("MintToken() expected error for omitted repos, got nil")
	}
	if !strings.Contains(err.Error(), "repos is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMintToken_CustomAudience(t *testing.T) {
	var gotAudience string
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAudience = r.URL.Query().Get("audience")
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()

	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(MintResult{Token: "tok", ExpiresAt: "2026-01-01T00:00:00Z"})
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?dummy=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL:  mintServer.URL,
		Role:     "triage",
		Repos:    []string{"repo"},
		Audience: "custom-aud",
	})
	if err != nil {
		t.Fatalf("MintToken() error = %v", err)
	}
	if gotAudience != "custom-aud" {
		t.Errorf("audience = %q, want %q", gotAudience, "custom-aud")
	}
}

func TestMintToken_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		req  MintRequest
		want string
	}{
		{"empty mint URL", MintRequest{Role: "triage", Repos: []string{"r"}}, "mint URL is required"},
		{"non-HTTPS mint URL", MintRequest{MintURL: "http://example.com", Role: "triage", Repos: []string{"r"}}, "mint URL must use HTTPS"},
		{"empty role", MintRequest{MintURL: "https://mint.example.com", Repos: []string{"r"}}, "role is required"},
		{"empty repos", MintRequest{MintURL: "https://mint.example.com", Role: "triage"}, "repos is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MintToken(context.Background(), tt.req)
			if err == nil {
				t.Fatal("expected error")
			}
			if got := err.Error(); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMintToken_MissingOIDCEnvVars(t *testing.T) {
	origEnv := envLookup
	defer func() { envLookup = origEnv }()

	t.Run("missing request URL", func(t *testing.T) {
		envLookup = func(key string) string { return "" }
		_, err := MintToken(context.Background(), MintRequest{
			MintURL: "https://mint.example.com",
			Role:    "triage",
			Repos:   []string{"r"},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "ACTIONS_ID_TOKEN_REQUEST_URL not set") {
			t.Errorf("error = %q, want to contain ACTIONS_ID_TOKEN_REQUEST_URL", err.Error())
		}
	})

	t.Run("missing request token", func(t *testing.T) {
		envLookup = func(key string) string {
			if key == "ACTIONS_ID_TOKEN_REQUEST_URL" {
				return "http://oidc"
			}
			return ""
		}
		_, err := MintToken(context.Background(), MintRequest{
			MintURL: "https://mint.example.com",
			Role:    "triage",
			Repos:   []string{"r"},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "ACTIONS_ID_TOKEN_REQUEST_TOKEN not set") {
			t.Errorf("error = %q, want to contain ACTIONS_ID_TOKEN_REQUEST_TOKEN", err.Error())
		}
	})
}

func TestMintToken_OIDCEndpointFailure(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer oidcServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: "https://mint.example.com",
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want to contain HTTP 500", err.Error())
	}
}

func TestMintToken_OIDCEmptyToken(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: ""})
	}))
	defer oidcServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: "https://mint.example.com",
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Errorf("error = %q, want to contain 'empty token'", err.Error())
	}
}

func TestMintToken_OIDCNetworkError(t *testing.T) {
	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return "http://127.0.0.1:1?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: "https://mint.example.com",
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "requesting OIDC token") {
		t.Errorf("error = %q, want to contain 'requesting OIDC token'", err.Error())
	}
}

func TestMintToken_OIDCInvalidJSON(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer oidcServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: "https://mint.example.com",
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "parsing response") {
		t.Errorf("error = %q, want to contain 'parsing response'", err.Error())
	}
}

func TestMintToken_MintNetworkError(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: "http://127.0.0.1:1",
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "requesting token") {
		t.Errorf("error = %q, want to contain 'requesting token'", err.Error())
	}
}

func TestMintToken_MintServiceFailure(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()

	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantSubstr string
	}{
		{
			"401 unauthorized",
			func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			"HTTP 401",
		},
		{
			"500 internal error",
			func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
			"HTTP 500",
		},
		{
			"empty token in response",
			func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(MintResult{Token: ""})
			},
			"empty token",
		},
		{
			"invalid JSON",
			func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("not json"))
			},
			"parsing response",
		},
	}

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mintServer := httptest.NewServer(tt.handler)
			defer mintServer.Close()

			_, err := MintToken(context.Background(), MintRequest{
				MintURL: mintServer.URL,
				Role:    "triage",
				Repos:   []string{"r"},
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

func TestMintToken_RetriesOn500(t *testing.T) {
	var attempts int
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()

	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(MintResult{Token: "tok", ExpiresAt: "2026-01-01T00:00:00Z"})
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	result, err := MintToken(context.Background(), MintRequest{
		MintURL: mintServer.URL,
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if result.Token != "tok" {
		t.Errorf("token = %q, want %q", result.Token, "tok")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestMintToken_RetryRespectsContextCancel(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()

	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := MintToken(ctx, MintRequest{
		MintURL: mintServer.URL,
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMintToken_DoesNotRetryOn4xx(t *testing.T) {
	var attempts int
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()

	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: mintServer.URL,
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("4xx should not retry: attempts = %d, want 1", attempts)
	}
}

func TestMintToken_LevelPassedToMint(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "oidc-jwt-value"})
	}))
	defer oidcServer.Close()

	var gotLevel string
	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body mintRequestBody
		json.NewDecoder(r.Body).Decode(&body)
		gotLevel = body.Level
		json.NewEncoder(w).Encode(MintResult{Token: "tok", ExpiresAt: "2026-01-01T00:00:00Z"})
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?dummy=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "test-request-token"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: mintServer.URL,
		Role:    "coder",
		Level:   "write",
		Repos:   []string{"my-repo"},
	})
	if err != nil {
		t.Fatalf("MintToken() error = %v", err)
	}
	if gotLevel != "write" {
		t.Errorf("level = %q, want %q", gotLevel, "write")
	}
}

func TestMintToken_LevelOmitted(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "oidc-jwt-value"})
	}))
	defer oidcServer.Close()

	var gotLevel string
	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body mintRequestBody
		json.NewDecoder(r.Body).Decode(&body)
		gotLevel = body.Level
		json.NewEncoder(w).Encode(MintResult{Token: "tok", ExpiresAt: "2026-01-01T00:00:00Z"})
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?dummy=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "test-request-token"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: mintServer.URL,
		Role:    "coder",
		Repos:   []string{"my-repo"},
	})
	if err != nil {
		t.Fatalf("MintToken() error = %v", err)
	}
	// Level should be empty when not specified (server defaults to "write" — temporary compatibility default).
	if gotLevel != "" {
		t.Errorf("level = %q, want empty (omitted)", gotLevel)
	}
}

func TestMintToken_LocalhostHTTPAllowed(t *testing.T) {
	origEnv := envLookup
	envLookup = func(key string) string { return "" }
	defer func() { envLookup = origEnv }()

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: "http://localhost:8080",
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("localhost HTTP should be allowed, got: %v", err)
	}
}

// TestMintToken_SurvivesTransientFailuresWithinMaxMintDuration proves a
// caller bounding MintToken with a context.WithTimeout(ctx, MaxMintDuration)
// deadline (as the post-script remint does, #7231) survives several
// transient 5xx responses across both the OIDC exchange and the mint call,
// rather than the deadline cutting retries short mid-backoff. retryBaseDelay
// is shortened (from the package-wide 0 set by init, to a small positive
// value here) so the exponential backoff between attempts actually runs,
// instead of the test passing vacuously with instantaneous retries.
func TestMintToken_SurvivesTransientFailuresWithinMaxMintDuration(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = 10 * time.Millisecond
	defer func() { retryBaseDelay = origDelay }()

	var oidcAttempts int
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oidcAttempts++
		if oidcAttempts < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()

	var mintAttempts int
	mintServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mintAttempts++
		if mintAttempts < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(MintResult{Token: "ghu_after_retries", ExpiresAt: "2026-01-01T00:00:00Z"})
	}))
	defer mintServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	ctx, cancel := context.WithTimeout(context.Background(), MaxMintDuration)
	defer cancel()

	start := time.Now()
	result, err := MintToken(ctx, MintRequest{
		MintURL: mintServer.URL,
		Role:    "triage",
		Repos:   []string{"r"},
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("MintToken() error = %v, want success after transient failures within MaxMintDuration", err)
	}
	if result.Token != "ghu_after_retries" {
		t.Errorf("token = %q, want %q", result.Token, "ghu_after_retries")
	}
	if oidcAttempts != 2 {
		t.Errorf("oidcAttempts = %d, want 2 (one transient 5xx then success)", oidcAttempts)
	}
	if mintAttempts != 3 {
		t.Errorf("mintAttempts = %d, want 3 (two transient 5xx then success)", mintAttempts)
	}
	if elapsed >= MaxMintDuration {
		t.Errorf("MintToken took %s, want comfortably under MaxMintDuration (%s)", elapsed, MaxMintDuration)
	}
}

func TestDefaultAudienceMatchesMintconsts(t *testing.T) {
	t.Parallel()
	if defaultAudience != mintconsts.OIDCAudience {
		t.Fatalf("defaultAudience = %q, want mintconsts.OIDCAudience %q", defaultAudience, mintconsts.OIDCAudience)
	}
}

func TestQueryStatus_OIDCPath(t *testing.T) {
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "oidc-jwt"})
	}))
	defer oidcServer.Close()

	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/status" {
			t.Errorf("path = %q, want /v1/status", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oidc-jwt" {
			t.Errorf("auth = %q, want %q", got, "Bearer oidc-jwt")
		}
		json.NewEncoder(w).Encode(StatusResult{
			Org:     "acme",
			Roles:   []string{"coder", "triage"},
			Version: "1.0.0",
		})
	}))
	defer statusServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	result, method, err := QueryStatus(context.Background(), StatusRequest{
		MintURL: statusServer.URL,
	}, nil)
	if err != nil {
		t.Fatalf("QueryStatus() error = %v", err)
	}
	if method != StatusAuthOIDC {
		t.Errorf("method = %q, want %q", method, StatusAuthOIDC)
	}
	if result.Org != "acme" {
		t.Errorf("org = %q, want %q", result.Org, "acme")
	}
	if len(result.Roles) != 2 {
		t.Errorf("roles = %v, want 2 roles", result.Roles)
	}
	if result.Version != "1.0.0" {
		t.Errorf("version = %q, want %q", result.Version, "1.0.0")
	}
}

func TestQueryStatus_GitHubTokenPath(t *testing.T) {
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghp-test" {
			t.Errorf("auth = %q, want %q", got, "Bearer ghp-test")
		}
		json.NewEncoder(w).Encode(StatusResult{
			Roles:             []string{"coder"},
			WorkflowHostRepos: []string{"fullsend-ai/fullsend"},
			Version:           "2.0.0",
		})
	}))
	defer statusServer.Close()

	// No OIDC env vars → falls through to GitHub token.
	origEnv := envLookup
	envLookup = func(key string) string { return "" }
	defer func() { envLookup = origEnv }()

	result, method, err := QueryStatus(context.Background(), StatusRequest{
		MintURL: statusServer.URL,
	}, func() (string, error) { return "ghp-test", nil })
	if err != nil {
		t.Fatalf("QueryStatus() error = %v", err)
	}
	if method != StatusAuthGitHub {
		t.Errorf("method = %q, want %q", method, StatusAuthGitHub)
	}
	if result.Org != "" {
		t.Errorf("org = %q, want empty on the GitHub token path", result.Org)
	}
	if len(result.Roles) != 1 || result.Roles[0] != "coder" {
		t.Errorf("roles = %v, want [coder]", result.Roles)
	}
	if len(result.WorkflowHostRepos) != 1 || result.WorkflowHostRepos[0] != "fullsend-ai/fullsend" {
		t.Errorf("workflow_host_repos = %v, want [fullsend-ai/fullsend]", result.WorkflowHostRepos)
	}
	if result.Version != "2.0.0" {
		t.Errorf("version = %q, want %q", result.Version, "2.0.0")
	}
}

func TestQueryStatus_OIDCFallsBackToGitHub(t *testing.T) {
	var calls int
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer oidc-jwt" {
			// OIDC token rejected by server.
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "auth failed"})
			return
		}
		if auth == "Bearer ghp-fallback" {
			json.NewEncoder(w).Encode(StatusResult{
				Roles: []string{"triage"},
			})
			return
		}
		t.Errorf("unexpected auth header: %q", auth)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer statusServer.Close()

	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "oidc-jwt"})
	}))
	defer oidcServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	result, method, err := QueryStatus(context.Background(), StatusRequest{
		MintURL: statusServer.URL,
	}, func() (string, error) { return "ghp-fallback", nil })
	if err != nil {
		t.Fatalf("QueryStatus() error = %v", err)
	}
	if method != StatusAuthGitHub {
		t.Errorf("method = %q, want %q", method, StatusAuthGitHub)
	}
	if len(result.Roles) != 1 || result.Roles[0] != "triage" {
		t.Errorf("roles = %v, want [triage]", result.Roles)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one OIDC, one GitHub)", calls)
	}
}

func TestQueryStatus_OIDCContextCanceledSkipsGitHubFallback(t *testing.T) {
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call to status endpoint")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer statusServer.Close()

	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call to OIDC endpoint")
	}))
	defer oidcServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcServer.URL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	defer func() { envLookup = origEnv }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fallbackCalled := false
	_, _, err := QueryStatus(ctx, StatusRequest{
		MintURL: statusServer.URL,
	}, func() (string, error) {
		fallbackCalled = true
		return "ghp-test", nil
	})
	if err == nil {
		t.Fatal("expected error when context is canceled during OIDC fetch")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want to wrap context.Canceled", err)
	}
	if fallbackCalled {
		t.Error("resolveGitHubToken should not be called when OIDC fetch is canceled")
	}
}

func TestQueryStatus_AllMethodsFail(t *testing.T) {
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "auth failed"})
	}))
	defer statusServer.Close()

	// No OIDC env vars, and GitHub token also rejected.
	origEnv := envLookup
	envLookup = func(key string) string { return "" }
	defer func() { envLookup = origEnv }()

	_, _, err := QueryStatus(context.Background(), StatusRequest{
		MintURL: statusServer.URL,
	}, func() (string, error) { return "ghp-bad", nil })
	if err == nil {
		t.Fatal("expected error when all auth methods fail")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("error = %q, want to contain 'authentication failed'", err.Error())
	}
	if !strings.Contains(err.Error(), "OIDC") {
		t.Errorf("error = %q, want to list OIDC attempt", err.Error())
	}
	if !strings.Contains(err.Error(), "GitHub token") {
		t.Errorf("error = %q, want to list GitHub token attempt", err.Error())
	}
}

func TestQueryStatus_ResolveGitHubTokenErrors(t *testing.T) {
	// No OIDC env vars, and resolveGitHubToken itself returns an error
	// (as opposed to succeeding with an empty or rejected token).
	origEnv := envLookup
	envLookup = func(key string) string { return "" }
	defer func() { envLookup = origEnv }()

	_, _, err := QueryStatus(context.Background(), StatusRequest{
		MintURL: "https://mint.example.com",
	}, func() (string, error) { return "", fmt.Errorf("gh auth token: not logged in") })
	if err == nil {
		t.Fatal("expected error when resolveGitHubToken fails")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("error = %q, want to contain 'authentication failed'", err.Error())
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want to list the resolveGitHubToken error", err.Error())
	}
}

func TestQueryStatus_NoResolveFunc(t *testing.T) {
	// No OIDC and no resolveGitHubToken func.
	origEnv := envLookup
	envLookup = func(key string) string { return "" }
	defer func() { envLookup = origEnv }()

	_, _, err := QueryStatus(context.Background(), StatusRequest{
		MintURL: "https://mint.example.com",
	}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("error = %q, want to contain 'authentication failed'", err.Error())
	}
}

func TestQueryStatus_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		req  StatusRequest
		want string
	}{
		{"empty mint URL", StatusRequest{}, "mint URL is required"},
		{"non-HTTPS mint URL", StatusRequest{MintURL: "http://example.com"}, "mint URL must use HTTPS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := QueryStatus(context.Background(), tt.req, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			if got := err.Error(); got != tt.want {
				t.Errorf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueryStatus_Non401ErrorIsTerminal(t *testing.T) {
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
	}))
	defer statusServer.Close()

	origEnv := envLookup
	envLookup = func(key string) string { return "" }
	defer func() { envLookup = origEnv }()

	ghCalled := false
	_, _, err := QueryStatus(context.Background(), StatusRequest{
		MintURL: statusServer.URL,
	}, func() (string, error) {
		ghCalled = true
		return "ghp-test", nil
	})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	// 403 from GitHub token should be terminal (not fall through).
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("error = %q, want to contain HTTP 403", err.Error())
	}
	// Since there are no OIDC env vars, GitHub token is the first (and only)
	// attempt. The 403 should terminate without trying anything else.
	if !ghCalled {
		t.Error("expected GitHub token resolver to be called")
	}
}

func TestHasOIDCEnv(t *testing.T) {
	origEnv := envLookup
	defer func() { envLookup = origEnv }()

	t.Run("both set", func(t *testing.T) {
		envLookup = func(key string) string {
			switch key {
			case "ACTIONS_ID_TOKEN_REQUEST_URL":
				return "http://oidc"
			case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
				return "tok"
			default:
				return ""
			}
		}
		if !hasOIDCEnv() {
			t.Error("hasOIDCEnv() = false, want true")
		}
	})

	t.Run("URL missing", func(t *testing.T) {
		envLookup = func(key string) string {
			if key == "ACTIONS_ID_TOKEN_REQUEST_TOKEN" {
				return "tok"
			}
			return ""
		}
		if hasOIDCEnv() {
			t.Error("hasOIDCEnv() = true, want false")
		}
	})

	t.Run("token missing", func(t *testing.T) {
		envLookup = func(key string) string {
			if key == "ACTIONS_ID_TOKEN_REQUEST_URL" {
				return "http://oidc"
			}
			return ""
		}
		if hasOIDCEnv() {
			t.Error("hasOIDCEnv() = true, want false")
		}
	})

	t.Run("neither set", func(t *testing.T) {
		envLookup = func(key string) string { return "" }
		if hasOIDCEnv() {
			t.Error("hasOIDCEnv() = true, want false")
		}
	})
}

// useOIDCServer points the OIDC env vars at oidcURL for the duration of t.
func useOIDCServer(t *testing.T, oidcURL string) {
	t.Helper()
	origEnv := envLookup
	envLookup = func(key string) string {
		switch key {
		case "ACTIONS_ID_TOKEN_REQUEST_URL":
			return oidcURL + "?d=1"
		case "ACTIONS_ID_TOKEN_REQUEST_TOKEN":
			return "tok"
		default:
			return ""
		}
	}
	t.Cleanup(func() { envLookup = origEnv })
}

func newMintServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(MintResult{Token: "tok", ExpiresAt: "2026-01-01T00:00:00Z"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMintToken_OIDCRetriesThroughLongOutage covers #8276: the OIDC
// endpoint returning 503 for most of the retry budget must not fail the
// mint as long as it recovers before the budget runs out.
func TestMintToken_OIDCRetriesThroughLongOutage(t *testing.T) {
	var oidcAttempts atomic.Int32
	failures := int32(oidcRetry.maxAttempts - 1)
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oidcAttempts.Add(1) <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "jwt"})
	}))
	defer oidcServer.Close()
	useOIDCServer(t, oidcServer.URL)

	result, err := MintToken(context.Background(), MintRequest{
		MintURL: newMintServer(t).URL,
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err != nil {
		t.Fatalf("MintToken() error = %v, want success after %d OIDC 503s", err, failures)
	}
	if result.Token != "tok" {
		t.Errorf("token = %q, want %q", result.Token, "tok")
	}
	if got := oidcAttempts.Load(); got != failures+1 {
		t.Errorf("oidcAttempts = %d, want %d", got, failures+1)
	}
}

func TestMintToken_OIDCPersistent503ReportsAttempts(t *testing.T) {
	var oidcAttempts atomic.Int32
	oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oidcAttempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer oidcServer.Close()
	useOIDCServer(t, oidcServer.URL)

	_, err := MintToken(context.Background(), MintRequest{
		MintURL: newMintServer(t).URL,
		Role:    "triage",
		Repos:   []string{"r"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := int(oidcAttempts.Load()); got != oidcRetry.maxAttempts {
		t.Errorf("oidcAttempts = %d, want %d", got, oidcRetry.maxAttempts)
	}
	wantAttempts := fmt.Sprintf("after %d attempts", oidcRetry.maxAttempts)
	if !strings.Contains(err.Error(), wantAttempts) {
		t.Errorf("error = %q, want to contain %q", err.Error(), wantAttempts)
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error = %q, want to contain last status HTTP 503", err.Error())
	}
}

func TestMintToken_OIDC4xxNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var oidcAttempts atomic.Int32
			oidcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				oidcAttempts.Add(1)
				w.WriteHeader(status)
			}))
			defer oidcServer.Close()
			useOIDCServer(t, oidcServer.URL)

			_, err := MintToken(context.Background(), MintRequest{
				MintURL: newMintServer(t).URL,
				Role:    "triage",
				Repos:   []string{"r"},
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if got := oidcAttempts.Load(); got != 1 {
				t.Errorf("oidcAttempts = %d, want 1 (no retry on HTTP %d)", got, status)
			}
			if want := fmt.Sprintf("HTTP %d", status); !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want to contain %q", err.Error(), want)
			}
		})
	}
}

// TestOIDCRetryFitsMaxMintDuration checks the production schedules: the
// OIDC retry window covers about a minute, and that window plus one final
// OIDC attempt hanging for the full client timeout plus callMint's
// backoff stays under MaxMintDuration.
func TestOIDCRetryFitsMaxMintDuration(t *testing.T) {
	if oidcRetry.window < 55*time.Second {
		t.Errorf("oidcRetry.window = %s, want about a minute (>= 55s)", oidcRetry.window)
	}

	// Walk oidcRetry's schedule with fast failures at the production base
	// delay of 1s: the last attempt must start at the window boundary, and
	// the window, not maxAttempts, must be what ends the retries.
	origDelay := retryBaseDelay
	retryBaseDelay = time.Second
	defer func() { retryBaseDelay = origDelay }()
	var lastStart time.Duration
	attempts := 1
	for ; attempts < oidcRetry.maxAttempts; attempts++ {
		d, ok := oidcRetry.nextDelay(attempts, lastStart)
		if !ok {
			break
		}
		lastStart += d
	}
	if lastStart != oidcRetry.window {
		t.Errorf("fast-failing OIDC retries start their last attempt at %s, want the window boundary %s", lastStart, oidcRetry.window)
	}
	if attempts >= oidcRetry.maxAttempts {
		t.Errorf("fast-failing OIDC retries used all %d attempts; maxAttempts, not the window, ended them", oidcRetry.maxAttempts)
	}

	var mintBackoff time.Duration
	for i := 0; i < callMintMaxAttempts-1; i++ {
		mintBackoff += time.Duration(1<<uint(i)) * time.Second
	}

	clientTimeout := httpClient.(*http.Client).Timeout
	total := oidcRetry.window + clientTimeout + mintBackoff
	if total >= MaxMintDuration {
		t.Errorf("OIDC window %s + client timeout %s + callMint backoff %s = %s, want < MaxMintDuration (%s)",
			oidcRetry.window, clientTimeout, mintBackoff, total, MaxMintDuration)
	}
}

func TestDoWithRetryPolicy_WindowStopsSlowRetries(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = origDelay }()

	attempts := 0
	start := time.Now()
	// 10ms attempts with backoff doubling from 1ms fit about 8 attempts in
	// a 200ms window; maxAttempts is far higher so only the window stops
	// the loop.
	err := doWithRetryPolicy(context.Background(), retryPolicy{maxAttempts: 100, window: 200 * time.Millisecond}, func() error {
		attempts++
		time.Sleep(10 * time.Millisecond)
		return &retryableError{errors.New("slow 503")}
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error")
	}
	if attempts < 2 || attempts >= 20 {
		t.Errorf("attempts = %d, want 2-19 within a 200ms window of 10ms attempts", attempts)
	}
	if want := fmt.Sprintf("giving up after %d attempts", attempts); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want to contain %q", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "slow 503") {
		t.Errorf("error = %q, want to wrap last error", err.Error())
	}
	if elapsed > 2*time.Second {
		t.Errorf("doWithRetryPolicy took %s, want the window to bound it", elapsed)
	}
}

func TestDoWithRetryPolicy_MaxDelayCapsBackoff(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Hour
	defer func() { retryBaseDelay = origDelay }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	attempts := 0
	err := doWithRetryPolicy(ctx, retryPolicy{maxAttempts: 3, maxDelay: time.Millisecond}, func() error {
		attempts++
		return &retryableError{errors.New("503")}
	})
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("doWithRetryPolicy() error = %v; maxDelay did not cap the 1h base delay", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if err == nil || !strings.Contains(err.Error(), "giving up after 3 attempts") {
		t.Errorf("error = %v, want 'giving up after 3 attempts'", err)
	}
}

func TestDoWithRetryPolicy_ContextCancelDuringBackoff(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Hour
	defer func() { retryBaseDelay = origDelay }()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)

	start := time.Now()
	err := doWithRetryPolicy(ctx, oidcRetry, func() error {
		return &retryableError{errors.New("503")}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("doWithRetryPolicy took %s after cancel, want prompt return", elapsed)
	}
}

func TestRetryPolicyNextDelay(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Second
	defer func() { retryBaseDelay = origDelay }()

	tests := []struct {
		name     string
		p        retryPolicy
		attempts int
		elapsed  time.Duration
		want     time.Duration
		wantOK   bool
	}{
		{"doubles from base", oidcRetry, 3, 3 * time.Second, 4 * time.Second, true},
		{"capped at maxDelay", oidcRetry, 6, 23 * time.Second, 8 * time.Second, true},
		// The #8276 review finding: after the attempt at ~55s the full 8s
		// backoff would overshoot the window, so it is shortened to 5s.
		{"shortened to reach window", oidcRetry, 10, 55 * time.Second, 5 * time.Second, true},
		{"window reached", oidcRetry, 11, 60 * time.Second, 0, false},
		{"window passed by slow attempt", oidcRetry, 5, 65 * time.Second, 0, false},
		{"no window never shortens", retryPolicy{maxAttempts: 5}, 4, time.Hour, 8 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.p.nextDelay(tt.attempts, tt.elapsed)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("nextDelay(%d, %s) = (%s, %v), want (%s, %v)", tt.attempts, tt.elapsed, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestDoWithRetryPolicy_RecoversAtWindowEnd runs oidcRetry's schedule at
// 1/100 scale (1s -> 10ms) against an endpoint that recovers between the
// ~55s attempt and the 60s window end. Before the final backoff was
// shortened to the window, this outage failed the mint.
func TestDoWithRetryPolicy_RecoversAtWindowEnd(t *testing.T) {
	const scale = 100
	origDelay := retryBaseDelay
	retryBaseDelay = time.Second / scale
	defer func() { retryBaseDelay = origDelay }()

	p := retryPolicy{
		maxAttempts: oidcRetry.maxAttempts,
		maxDelay:    oidcRetry.maxDelay / scale,
		window:      oidcRetry.window / scale,
	}
	recoverAt := 58 * time.Second / scale

	start := time.Now()
	attempts := 0
	err := doWithRetryPolicy(context.Background(), p, func() error {
		attempts++
		if time.Since(start) < recoverAt {
			return &retryableError{errors.New("503")}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("doWithRetryPolicy() error = %v after %d attempts, want success once the endpoint recovers at %s (window %s)",
			err, attempts, recoverAt, p.window)
	}
}
