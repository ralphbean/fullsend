package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fullsend-ai/fullsend/internal/mintcore"
)

type testKubernetesPEMAccessor struct{ data []byte }

func (a testKubernetesPEMAccessor) AccessPEM(context.Context, string) ([]byte, error) {
	return append([]byte(nil), a.data...), nil
}

type failingKubernetesPEMAccessor struct{}

func (failingKubernetesPEMAccessor) AccessPEM(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("test PEM failure")
}

func TestNewKubernetesMintHandlerRejectsInvalidConfigurationInputs(t *testing.T) {
	_, jwksPath := writeTestKubernetesJWKS(t)
	policy := testKubernetesMintConfig(jwksPath)
	policyData, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, policyData, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		path      string
		policy    string
		appIDs    string
		pem       mintcore.PEMAccessor
		wantError string
	}{
		{name: "missing policy path", wantError: "KUBERNETES_MINT_CONFIG_FILE"},
		{name: "unreadable policy", path: filepath.Join(t.TempDir(), "missing.json"), wantError: "reading Kubernetes mint policy"},
		{name: "invalid JSON", path: policyPath, policy: `{`, wantError: "decoding Kubernetes mint policy"},
		{name: "unknown policy field", path: policyPath, policy: string(policyData[:len(policyData)-1]) + `,"unexpected":true}`, wantError: "decoding Kubernetes mint policy"},
		{name: "trailing policy value", path: policyPath, policy: string(policyData) + `{}`, wantError: "trailing JSON value"},
		{name: "invalid policy", path: policyPath, policy: `{}`, appIDs: `{"coder":"456"}`, wantError: "invalid Kubernetes mint policy"},
		{name: "invalid App ID JSON", path: policyPath, policy: string(policyData), appIDs: `{`, wantError: "ROLE_APP_IDS"},
		{name: "missing role App ID", path: policyPath, policy: string(policyData), appIDs: `{}`, pem: testKubernetesPEMAccessor{}, wantError: "no matching ROLE_APP_IDS"},
		{name: "missing public keys", path: policyPath, appIDs: `{"coder":"456"}`, pem: testKubernetesPEMAccessor{}, wantError: "loading signing keys"},
		{name: "missing PEM accessor", path: policyPath, policy: string(policyData), appIDs: `{"coder":"456"}`, wantError: "PEM accessor is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if tc.name == "missing public keys" {
				invalidPolicy := testKubernetesMintConfig(filepath.Join(t.TempDir(), "absent-jwks.json"))
				data, marshalErr := json.Marshal(invalidPolicy)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.policy != "" {
				if err := os.WriteFile(path, []byte(tc.policy), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.appIDs != "" {
				t.Setenv("ROLE_APP_IDS", tc.appIDs)
			} else {
				t.Setenv("ROLE_APP_IDS", `{"coder":"456"}`)
			}
			_, gotErr := newKubernetesMintHandler(path, tc.pem)
			if gotErr == nil || !strings.Contains(gotErr.Error(), tc.wantError) {
				t.Fatalf("newKubernetesMintHandler() error = %v, want %q", gotErr, tc.wantError)
			}
		})
	}
}

func TestKubernetesMintHandlerScopesTokenToSignedWorkloadIdentity(t *testing.T) {
	privateKey, jwksPath := writeTestKubernetesJWKS(t)
	handler := testKubernetesMintHandler(t, jwksPath, testKubernetesAppPEM(t))
	var exchangeCalls int
	handler.exchangeInstallationToken = func(_ context.Context, baseURL, appJWT string, installationID int64, role, level string, repositoryIDs []int64) (string, string, *mintcore.GrantedScope, error) {
		exchangeCalls++
		if baseURL != "https://api.github.com" || appJWT == "" || installationID != 9876 || role != "coder" || level != "write" {
			t.Errorf("unexpected exchange input: base=%q installation=%d role=%q level=%q", baseURL, installationID, role, level)
		}
		if len(repositoryIDs) != 1 || repositoryIDs[0] != 123456 {
			t.Errorf("repository IDs = %v, want [123456]", repositoryIDs)
		}
		return "ghs_scoped", "2099-01-01T00:00:00Z", &mintcore.GrantedScope{
			RepositoryIDs: []int64{123456}, Permissions: testKubernetesWritePermissions(t),
		}, nil
	}

	request := httptest.NewRequest(http.MethodPost, kubernetesMintPath, strings.NewReader(`{"repository_id":123456,"role":"coder","level":"write"}`))
	request.Header.Set("Authorization", "Bearer "+signKubernetesJWT(t, privateKey, testKubernetesClaims(time.Now(), "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var got kubernetesMintResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Token != "ghs_scoped" || got.ExpiresAt != "2099-01-01T00:00:00Z" || exchangeCalls != 1 ||
		!reflect.DeepEqual(got.RepositoryIDs, []int64{123456}) || !reflect.DeepEqual(got.Permissions, testKubernetesWritePermissions(t)) {
		t.Fatalf("response=%+v exchange calls=%d", got, exchangeCalls)
	}
}

func TestKubernetesMintHandlerRejectsWrongIdentityAndScope(t *testing.T) {
	privateKey, jwksPath := writeTestKubernetesJWKS(t)
	handler := testKubernetesMintHandler(t, jwksPath, testKubernetesAppPEM(t))
	exchangeCalls := 0
	handler.exchangeInstallationToken = func(context.Context, string, string, int64, string, string, []int64) (string, string, *mintcore.GrantedScope, error) {
		exchangeCalls++
		return "ghs_should_not_be_created", "", nil, nil
	}
	now := time.Now()
	cases := []struct {
		name       string
		claims     kubernetesJWTClaims
		request    string
		wantStatus int
	}{
		{
			name:    "wrong issuer",
			claims:  testKubernetesClaims(now, "https://other-issuer.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder"),
			request: `{"repository_id":123456,"role":"coder","level":"write"}`, wantStatus: http.StatusUnauthorized,
		},
		{
			name:    "wrong audience",
			claims:  testKubernetesClaims(now, "https://kind.fullsend.test", "google-wif-audience", "fullsend-team-a", "fs-repo-123456-coder"),
			request: `{"repository_id":123456,"role":"coder","level":"write"}`, wantStatus: http.StatusUnauthorized,
		},
		{
			name:    "wrong namespace",
			claims:  testKubernetesClaims(now, "https://kind.fullsend.test", kubernetesMintAudience, "other-namespace", "fs-repo-123456-coder"),
			request: `{"repository_id":123456,"role":"coder","level":"write"}`, wantStatus: http.StatusUnauthorized,
		},
		{
			name:    "namespace subject mismatch",
			claims:  testKubernetesClaims(now, "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-b", "fs-repo-123456-coder"),
			request: `{"repository_id":123456,"role":"coder","level":"write"}`, wantStatus: http.StatusUnauthorized,
		},
		{
			name:    "wrong service account role",
			claims:  testKubernetesClaims(now, "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-review"),
			request: `{"repository_id":123456,"role":"review","level":"write"}`, wantStatus: http.StatusForbidden,
		},
		{
			name:    "request repository does not match service account",
			claims:  testKubernetesClaims(now, "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder"),
			request: `{"repository_id":654321,"role":"coder","level":"write"}`, wantStatus: http.StatusForbidden,
		},
		{
			name:    "service account repository is outside configured ceiling",
			claims:  testKubernetesClaims(now, "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-654321-coder"),
			request: `{"repository_id":654321,"role":"coder","level":"write"}`, wantStatus: http.StatusForbidden,
		},
		{
			name:    "role ceiling denies write",
			claims:  testKubernetesClaims(now, "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder"),
			request: `{"repository_id":123456,"role":"coder","level":"admin"}`, wantStatus: http.StatusForbidden,
		},
		{
			name:    "expired token",
			claims:  testKubernetesClaims(now.Add(-2*time.Hour), "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder"),
			request: `{"repository_id":123456,"role":"coder","level":"write"}`, wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, kubernetesMintPath, strings.NewReader(tc.request))
			request.Header.Set("Authorization", "Bearer "+signKubernetesJWT(t, privateKey, tc.claims))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
		})
	}
	if exchangeCalls != 0 {
		t.Fatalf("GitHub exchange called %d times for denied requests", exchangeCalls)
	}
}

func TestKubernetesMintHandlerRejectsBroaderGitHubGrant(t *testing.T) {
	privateKey, jwksPath := writeTestKubernetesJWKS(t)
	handler := testKubernetesMintHandler(t, jwksPath, testKubernetesAppPEM(t))
	handler.exchangeInstallationToken = func(context.Context, string, string, int64, string, string, []int64) (string, string, *mintcore.GrantedScope, error) {
		return "ghs_broader_token", "2099-01-01T00:00:00Z", &mintcore.GrantedScope{
			RepositoryIDs: []int64{123456, 654321}, Permissions: testKubernetesWritePermissions(t),
		}, nil
	}
	request := httptest.NewRequest(http.MethodPost, kubernetesMintPath, strings.NewReader(`{"repository_id":123456,"role":"coder","level":"write"}`))
	request.Header.Set("Authorization", "Bearer "+signKubernetesJWT(t, privateKey, testKubernetesClaims(time.Now(), "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "ghs_broader_token") {
		t.Fatalf("broader GitHub grant was not rejected safely: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestKubernetesMintHandlerRejectsPermissionMismatches(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions map[string]string
	}{
		{name: "missing write", permissions: map[string]string{"contents": "read"}},
		{name: "broader permission", permissions: map[string]string{
			"contents": "write", "packages": "read", "pull_requests": "write", "issues": "write",
			"checks": "read", "metadata": "read", "administration": "write",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			privateKey, jwksPath := writeTestKubernetesJWKS(t)
			handler := testKubernetesMintHandler(t, jwksPath, testKubernetesAppPEM(t))
			handler.exchangeInstallationToken = func(context.Context, string, string, int64, string, string, []int64) (string, string, *mintcore.GrantedScope, error) {
				return "ghs_mismatched_scope", "2099-01-01T00:00:00Z", &mintcore.GrantedScope{
					RepositoryIDs: []int64{123456}, Permissions: tc.permissions,
				}, nil
			}
			request := httptest.NewRequest(http.MethodPost, kubernetesMintPath, strings.NewReader(`{"repository_id":123456,"role":"coder","level":"write"}`))
			request.Header.Set("Authorization", "Bearer "+signKubernetesJWT(t, privateKey, testKubernetesClaims(time.Now(), "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder")))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "ghs_mismatched_scope") {
				t.Fatalf("mismatched GitHub permission grant was not rejected safely: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestKubernetesMintHandlerRejectsMalformedRequestsAndCredentialFailures(t *testing.T) {
	privateKey, jwksPath := writeTestKubernetesJWKS(t)
	validJWT := signKubernetesJWT(t, privateKey, testKubernetesClaims(time.Now(), "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder"))
	validBody := `{"repository_id":123456,"role":"coder","level":"write"}`
	makeHandler := func(pem mintcore.PEMAccessor) *kubernetesMintHandler {
		h := testKubernetesMintHandler(t, jwksPath, testKubernetesAppPEM(t))
		if pem != nil {
			h.pemAccessor = pem
		}
		h.exchangeInstallationToken = func(context.Context, string, string, int64, string, string, []int64) (string, string, *mintcore.GrantedScope, error) {
			return "ghs_scoped", "2099-01-01T00:00:00Z", &mintcore.GrantedScope{
				RepositoryIDs: []int64{123456}, Permissions: testKubernetesWritePermissions(t),
			}, nil
		}
		return h
	}
	tests := []struct {
		name       string
		method     string
		body       string
		authorize  bool
		pem        mintcore.PEMAccessor
		mutate     func(*kubernetesMintHandler)
		wantStatus int
	}{
		{name: "method", method: http.MethodGet, body: validBody, authorize: true, wantStatus: http.StatusMethodNotAllowed},
		{name: "missing bearer", method: http.MethodPost, body: validBody, wantStatus: http.StatusUnauthorized},
		{name: "malformed request", method: http.MethodPost, body: `{`, authorize: true, wantStatus: http.StatusBadRequest},
		{name: "unknown request field", method: http.MethodPost, body: `{"repository_id":123456,"role":"coder","level":"write","installation_id":1}`, authorize: true, wantStatus: http.StatusBadRequest},
		{name: "trailing request value", method: http.MethodPost, body: validBody + `{}`, authorize: true, wantStatus: http.StatusBadRequest},
		{name: "oversized request", method: http.MethodPost, body: strings.Repeat(" ", 17*1024), authorize: true, wantStatus: http.StatusBadRequest},
		{name: "PEM unavailable", method: http.MethodPost, body: validBody, authorize: true, pem: failingKubernetesPEMAccessor{}, wantStatus: http.StatusForbidden},
		{name: "App key invalid", method: http.MethodPost, body: validBody, authorize: true, pem: testKubernetesPEMAccessor{data: []byte("invalid")}, wantStatus: http.StatusInternalServerError},
		{name: "role App ID missing", method: http.MethodPost, body: validBody, authorize: true, mutate: func(h *kubernetesMintHandler) { h.appIDs = nil }, wantStatus: http.StatusForbidden},
		{name: "GitHub exchange failure", method: http.MethodPost, body: validBody, authorize: true, mutate: func(h *kubernetesMintHandler) {
			h.exchangeInstallationToken = func(context.Context, string, string, int64, string, string, []int64) (string, string, *mintcore.GrantedScope, error) {
				return "", "", nil, fmt.Errorf("denied")
			}
		}, wantStatus: http.StatusBadGateway},
		{name: "GitHub omitted granted scope", method: http.MethodPost, body: validBody, authorize: true, mutate: func(h *kubernetesMintHandler) {
			h.exchangeInstallationToken = func(context.Context, string, string, int64, string, string, []int64) (string, string, *mintcore.GrantedScope, error) {
				return "ghs_scoped", "", nil, nil
			}
		}, wantStatus: http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := makeHandler(tc.pem)
			if tc.mutate != nil {
				tc.mutate(handler)
			}
			request := httptest.NewRequest(tc.method, kubernetesMintPath, strings.NewReader(tc.body))
			if tc.authorize {
				request.Header.Set("Authorization", "Bearer "+validJWT)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
		})
	}
}

func testKubernetesWritePermissions(t *testing.T) map[string]string {
	t.Helper()
	permissions, err := mintcore.RolePermissionsForLevel("coder", mintcore.LevelWrite)
	if err != nil {
		t.Fatal(err)
	}
	return permissions
}

func TestKubernetesMintReadsRotatedJWKSOnEachExchange(t *testing.T) {
	firstKey, jwksPath := writeTestKubernetesJWKS(t)
	handler := testKubernetesMintHandler(t, jwksPath, []byte("unused"))
	claims := testKubernetesClaims(time.Now(), "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder")
	if _, err := handler.authenticate(signKubernetesJWT(t, firstKey, claims)); err != nil {
		t.Fatalf("first trusted key rejected: %v", err)
	}
	secondKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	writeKubernetesJWKS(t, jwksPath, secondKey)
	if _, err := handler.authenticate(signKubernetesJWT(t, secondKey, claims)); err != nil {
		t.Fatalf("rotated trusted key rejected: %v", err)
	}
	if _, err := handler.authenticate(signKubernetesJWT(t, firstKey, claims)); err == nil {
		t.Fatal("old signing key accepted after JWKS rotation")
	}
}

func TestLoadKubernetesPublicKeysRejectsPrivateKeyMaterial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, []byte(`{"keys":[{"kty":"RSA","alg":"RS256","use":"sig","kid":"private","n":"AQ","e":"Aw","d":"secret"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKubernetesPublicKeys(path); err == nil || !strings.Contains(err.Error(), "private key material") {
		t.Fatalf("loadKubernetesPublicKeys() error = %v, want private key rejection", err)
	}
}

func TestLoadKubernetesPublicKeysRejectsMalformedAndUnsupportedSets(t *testing.T) {
	_, validPath := writeTestKubernetesJWKS(t)
	tests := []struct {
		name string
		data string
	}{
		{name: "malformed JSON", data: `{`},
		{name: "empty set", data: `{"keys":[]}`},
		{name: "unsupported key type", data: `{"keys":[{"kty":"EC","alg":"ES256","use":"sig","kid":"ec"}]}`},
		{name: "invalid modulus", data: `{"keys":[{"kty":"RSA","alg":"RS256","use":"sig","kid":"bad","n":"AQ","e":"Aw"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jwks.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadKubernetesPublicKeys(path); err == nil {
				t.Fatal("invalid JWKS was accepted")
			}
		})
	}
	if _, err := loadKubernetesPublicKeys(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing JWKS file was accepted")
	}
	tooLarge := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(tooLarge, make([]byte, maxKubernetesJWKSSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKubernetesPublicKeys(tooLarge); err == nil {
		t.Fatal("oversized JWKS was accepted")
	}
	var set kubernetesJWKSet
	data, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &set); err != nil {
		t.Fatal(err)
	}
	duplicatePath := filepath.Join(t.TempDir(), "duplicate.json")
	set.Keys = append(set.Keys, set.Keys[0])
	writeJWKSSet(t, duplicatePath, set)
	if _, err := loadKubernetesPublicKeys(duplicatePath); err == nil || !strings.Contains(err.Error(), "duplicate key ID") {
		t.Fatalf("duplicate key ID error = %v", err)
	}
	set.Keys = set.Keys[:1]
	set.Keys[0].Exponent = base64.RawURLEncoding.EncodeToString([]byte{2})
	exponentPath := filepath.Join(t.TempDir(), "bad-exponent.json")
	writeJWKSSet(t, exponentPath, set)
	if _, err := loadKubernetesPublicKeys(exponentPath); err == nil || !strings.Contains(err.Error(), "invalid RSA exponent") {
		t.Fatalf("invalid exponent error = %v", err)
	}
}

func writeJWKSSet(t *testing.T, path string, set kubernetesJWKSet) {
	t.Helper()
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testKubernetesMintHandler(t *testing.T, jwksPath string, appPEM []byte) *kubernetesMintHandler {
	t.Helper()
	config := testKubernetesMintConfig(jwksPath)
	issuer := config.TrustedIssuers[0]
	return &kubernetesMintHandler{
		issuers: map[string]kubernetesIssuerPolicy{issuer.Issuer: issuer},
		appIDs:  map[string]string{"coder": "456"}, pemAccessor: testKubernetesPEMAccessor{data: appPEM},
		exchangeInstallationToken: mintcore.CreateInstallationTokenForRepositoryIDs,
	}
}

func testKubernetesMintConfig(jwksPath string) kubernetesMintConfig {
	return kubernetesMintConfig{TrustedIssuers: []kubernetesIssuerPolicy{{
		Issuer: "https://kind.fullsend.test", Audience: kubernetesMintAudience, JWKSFile: jwksPath,
		Namespaces: map[string]kubernetesNamespacePolicy{
			"fullsend-team-a": {Tenant: "acme", Roles: map[string]kubernetesRolePolicy{
				"coder": {InstallationID: 9876, RepositoryIDs: []int64{123456}, AllowedLevels: []string{"read", "write"}},
			}},
		},
	}}}
}

func testKubernetesAppPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func testKubernetesClaims(now time.Time, issuer, audience, namespace, serviceAccount string) kubernetesJWTClaims {
	claims := kubernetesJWTClaims{
		Issuer: issuer, Subject: "system:serviceaccount:" + namespace + ":" + serviceAccount,
		Audience: mintcore.Audience{audience}, IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(10 * time.Minute).Unix(),
		Kubernetes: kubernetesJWTInfo{Namespace: namespace},
	}
	claims.Kubernetes.ServiceAccount.Name = serviceAccount
	claims.Kubernetes.ServiceAccount.UID = "service-account-uid"
	return claims
}

func writeTestKubernetesJWKS(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	writeKubernetesJWKS(t, path, privateKey)
	return privateKey, path
}

func writeKubernetesJWKS(t *testing.T, path string, key *rsa.PrivateKey) {
	t.Helper()
	exponent := big.NewInt(int64(key.PublicKey.E)).Bytes()
	set := kubernetesJWKSet{Keys: []kubernetesJWK{{
		KeyType: "RSA", Use: "sig", Alg: "RS256", KeyID: "kind-signing-key",
		Modulus:  base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		Exponent: base64.RawURLEncoding.EncodeToString(exponent),
	}}}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func signKubernetesJWT(t *testing.T, key *rsa.PrivateKey, claims kubernetesJWTClaims) string {
	t.Helper()
	return signKubernetesJWTWithHeader(t, key, claims, kubernetesJWTHeader{Algorithm: "RS256", KeyID: "kind-signing-key", Type: "JWT"})
}

func signKubernetesJWTWithHeader(t *testing.T, key *rsa.PrivateKey, claims kubernetesJWTClaims, jwtHeader kubernetesJWTHeader) string {
	t.Helper()
	header, err := json.Marshal(jwtHeader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestKubernetesAuthenticatorRejectsMalformedAndUnsupportedTokens(t *testing.T) {
	key, jwksPath := writeTestKubernetesJWKS(t)
	handler := testKubernetesMintHandler(t, jwksPath, []byte("unused"))
	claims := testKubernetesClaims(time.Now(), "https://kind.fullsend.test", kubernetesMintAudience, "fullsend-team-a", "fs-repo-123456-coder")
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		token string
	}{
		{name: "wrong segment count", token: "not-a-jwt"},
		{name: "invalid claims encoding", token: "e30.!invalid.signature"},
		{name: "unsupported algorithm", token: signKubernetesJWTWithHeader(t, key, claims, kubernetesJWTHeader{Algorithm: "HS256", KeyID: "kind-signing-key"})},
		{name: "missing key ID", token: signKubernetesJWTWithHeader(t, key, claims, kubernetesJWTHeader{Algorithm: "RS256"})},
		{name: "unknown key ID", token: signKubernetesJWTWithHeader(t, key, claims, kubernetesJWTHeader{Algorithm: "RS256", KeyID: "unknown"})},
		{name: "invalid signature", token: signKubernetesJWT(t, otherKey, claims)},
		{name: "missing times", token: signKubernetesJWT(t, key, kubernetesJWTClaims{Issuer: claims.Issuer, Subject: claims.Subject, Audience: claims.Audience, Kubernetes: claims.Kubernetes})},
		{name: "invalid service account naming", token: signKubernetesJWT(t, key, testKubernetesClaims(time.Now(), claims.Issuer, kubernetesMintAudience, "fullsend-team-a", "runner"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := handler.authenticate(tc.token); err == nil {
				t.Fatal("malformed or unsupported token was accepted")
			}
		})
	}
}

func TestKubernetesPolicyRejectsEmptyOrOverbroadConfiguration(t *testing.T) {
	good := kubernetesMintConfig{TrustedIssuers: []kubernetesIssuerPolicy{{
		Issuer: "https://kind.fullsend.test", Audience: kubernetesMintAudience, JWKSFile: "/tmp/jwks.json",
		Namespaces: map[string]kubernetesNamespacePolicy{"fullsend-team-a": {
			Tenant: "acme", Roles: map[string]kubernetesRolePolicy{"coder": {InstallationID: 9876, RepositoryIDs: []int64{123456}, AllowedLevels: []string{"read"}}},
		}},
	}}}
	if err := good.validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	mutations := []struct {
		name   string
		change func(*kubernetesMintConfig)
	}{
		{name: "no issuers", change: func(c *kubernetesMintConfig) { c.TrustedIssuers = nil }},
		{name: "non-HTTPS issuer", change: func(c *kubernetesMintConfig) { c.TrustedIssuers[0].Issuer = "http://kind.fullsend.test" }},
		{name: "duplicate issuer", change: func(c *kubernetesMintConfig) { c.TrustedIssuers = append(c.TrustedIssuers, c.TrustedIssuers[0]) }},
		{name: "wrong audience", change: func(c *kubernetesMintConfig) { c.TrustedIssuers[0].Audience = "*" }},
		{name: "relative JWKS path", change: func(c *kubernetesMintConfig) { c.TrustedIssuers[0].JWKSFile = "jwks.json" }},
		{name: "no namespaces", change: func(c *kubernetesMintConfig) { c.TrustedIssuers[0].Namespaces = nil }},
		{name: "invalid namespace", change: func(c *kubernetesMintConfig) {
			c.TrustedIssuers[0].Namespaces = map[string]kubernetesNamespacePolicy{"Team A": {Tenant: "acme", Roles: map[string]kubernetesRolePolicy{"coder": {InstallationID: 1, RepositoryIDs: []int64{123456}, AllowedLevels: []string{"read"}}}}}
		}},
		{name: "empty tenant", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			p.Tenant = " "
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
		{name: "no roles", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			p.Roles = nil
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
		{name: "invalid role", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			p.Roles = map[string]kubernetesRolePolicy{"invalid role": {InstallationID: 9876, RepositoryIDs: []int64{123456}, AllowedLevels: []string{"read"}}}
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
		{name: "missing installation", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			p.Roles["coder"] = kubernetesRolePolicy{RepositoryIDs: []int64{123456}, AllowedLevels: []string{"read"}}
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
		{name: "invalid repository ID", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			policy := p.Roles["coder"]
			policy.RepositoryIDs = []int64{0}
			p.Roles["coder"] = policy
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
		{name: "duplicate repository ID", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			policy := p.Roles["coder"]
			policy.RepositoryIDs = []int64{123456, 123456}
			p.Roles["coder"] = policy
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
		{name: "duplicate role level", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			policy := p.Roles["coder"]
			policy.AllowedLevels = []string{"read", "read"}
			p.Roles["coder"] = policy
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
		{name: "unknown role level", change: func(c *kubernetesMintConfig) {
			p := c.TrustedIssuers[0].Namespaces["fullsend-team-a"]
			p.Roles["coder"] = kubernetesRolePolicy{InstallationID: 9876, RepositoryIDs: []int64{123456}, AllowedLevels: []string{"admin"}}
			c.TrustedIssuers[0].Namespaces["fullsend-team-a"] = p
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			var config kubernetesMintConfig
			// Clone the maps so each case changes only its own policy value.
			data, _ := json.Marshal(good)
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			mutation.change(&config)
			if err := config.validate(); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestParsePositiveRepositoryIDRejectsNoncanonicalValues(t *testing.T) {
	for _, value := range []string{"0", "-1", "01", "1x", "999999999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parsePositiveRepositoryID(value); err == nil {
				t.Fatalf("parsePositiveRepositoryID(%q) succeeded", value)
			}
		})
	}
	if got, err := parsePositiveRepositoryID(fmt.Sprint(123456)); err != nil || got != 123456 {
		t.Fatalf("parsePositiveRepositoryID() = %d, %v", got, err)
	}
}
