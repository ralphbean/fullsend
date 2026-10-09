package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/mintcore"
)

const (
	kubernetesMintPath       = "/v1/kubernetes/token"
	kubernetesMintAudience   = "fullsend-k8s-mint"
	maxKubernetesJWKSSize    = 512 * 1024
	maxKubernetesTokenAge    = time.Hour
	kubernetesTokenClockSkew = 30 * time.Second
)

var (
	serviceAccountNamePattern  = regexp.MustCompile(`^fs-repo-([1-9][0-9]*)-([a-z][a-z0-9-]*)$`)
	kubernetesNamespacePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
)

// kubernetesMintConfig is operator-managed trust and tenant authorization.
// JWKS files contain public keys only and are read for each exchange so a
// projected ConfigMap update can replace keys without caching old trust.
type kubernetesMintConfig struct {
	TrustedIssuers []kubernetesIssuerPolicy `json:"trusted_issuers"`
}

type kubernetesIssuerPolicy struct {
	Issuer     string                               `json:"issuer"`
	Audience   string                               `json:"audience"`
	JWKSFile   string                               `json:"jwks_file"`
	Namespaces map[string]kubernetesNamespacePolicy `json:"namespaces"`
}

type kubernetesNamespacePolicy struct {
	Tenant string                          `json:"tenant"`
	Roles  map[string]kubernetesRolePolicy `json:"roles"`
}

type kubernetesRolePolicy struct {
	InstallationID int64    `json:"installation_id"`
	RepositoryIDs  []int64  `json:"repository_ids"`
	AllowedLevels  []string `json:"allowed_levels"`
}

type kubernetesJWTClaims struct {
	Issuer     string            `json:"iss"`
	Subject    string            `json:"sub"`
	Audience   mintcore.Audience `json:"aud"`
	IssuedAt   int64             `json:"iat"`
	NotBefore  int64             `json:"nbf"`
	ExpiresAt  int64             `json:"exp"`
	Kubernetes kubernetesJWTInfo `json:"kubernetes.io"`
}

type kubernetesJWTInfo struct {
	Namespace      string `json:"namespace"`
	ServiceAccount struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"serviceaccount"`
}

type kubernetesJWTHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ,omitempty"`
}

type kubernetesJWKSet struct {
	Keys []kubernetesJWK `json:"keys"`
}

type kubernetesJWK struct {
	KeyType     string          `json:"kty"`
	Use         string          `json:"use"`
	Alg         string          `json:"alg"`
	KeyID       string          `json:"kid"`
	Modulus     string          `json:"n"`
	Exponent    string          `json:"e"`
	D           string          `json:"d"`
	P           string          `json:"p"`
	Q           string          `json:"q"`
	DP          string          `json:"dp"`
	DQ          string          `json:"dq"`
	QI          string          `json:"qi"`
	OtherPrimes json.RawMessage `json:"oth,omitempty"`
}

type kubernetesMintRequest struct {
	RepositoryID int64  `json:"repository_id"`
	Role         string `json:"role"`
	Level        string `json:"level"`
}

type kubernetesMintResponse struct {
	Token         string            `json:"token"`
	ExpiresAt     string            `json:"expires_at"`
	RepositoryIDs []int64           `json:"repository_ids"`
	Permissions   map[string]string `json:"permissions"`
}

type verifiedKubernetesIdentity struct {
	issuer     kubernetesIssuerPolicy
	namespace  string
	repository int64
	role       string
}

type kubernetesMintHandler struct {
	issuers                   map[string]kubernetesIssuerPolicy
	appIDs                    map[string]string
	pemAccessor               mintcore.PEMAccessor
	exchangeInstallationToken func(context.Context, string, string, int64, string, string, []int64) (string, string, *mintcore.GrantedScope, error)
}

func newKubernetesMintHandler(configPath string, pemAccessor mintcore.PEMAccessor) (http.Handler, error) {
	if configPath == "" {
		return nil, fmt.Errorf("KUBERNETES_MINT_CONFIG_FILE is required")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading Kubernetes mint policy: %w", err)
	}
	var config kubernetesMintConfig
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decoding Kubernetes mint policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("decoding Kubernetes mint policy: trailing JSON value")
	}
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid Kubernetes mint policy: %w", err)
	}

	var configuredAppIDs map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("ROLE_APP_IDS")), &configuredAppIDs); err != nil {
		return nil, fmt.Errorf("parsing ROLE_APP_IDS for Kubernetes mint: %w", err)
	}
	appIDs := mintcore.RoleOnlyAppIDs(configuredAppIDs)
	for _, issuer := range config.TrustedIssuers {
		if _, err := loadKubernetesPublicKeys(issuer.JWKSFile); err != nil {
			return nil, fmt.Errorf("loading signing keys for issuer %q: %w", issuer.Issuer, err)
		}
		for _, namespace := range issuer.Namespaces {
			for role := range namespace.Roles {
				if appIDs[role] == "" {
					return nil, fmt.Errorf("Kubernetes mint role %q has no matching ROLE_APP_IDS entry", role)
				}
			}
		}
	}
	if pemAccessor == nil {
		return nil, fmt.Errorf("PEM accessor is required for Kubernetes mint")
	}
	issuers := make(map[string]kubernetesIssuerPolicy, len(config.TrustedIssuers))
	for _, issuer := range config.TrustedIssuers {
		issuers[issuer.Issuer] = issuer
	}
	return &kubernetesMintHandler{
		issuers: issuers, appIDs: appIDs, pemAccessor: pemAccessor,
		exchangeInstallationToken: mintcore.CreateInstallationTokenForRepositoryIDs,
	}, nil
}

func (c kubernetesMintConfig) validate() error {
	if len(c.TrustedIssuers) == 0 {
		return fmt.Errorf("trusted_issuers must not be empty")
	}
	seen := map[string]bool{}
	for _, issuer := range c.TrustedIssuers {
		parsed, err := url.Parse(issuer.Issuer)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("issuer must be a valid https URL")
		}
		if seen[issuer.Issuer] {
			return fmt.Errorf("duplicate trusted issuer %q", issuer.Issuer)
		}
		seen[issuer.Issuer] = true
		if issuer.Audience != kubernetesMintAudience {
			return fmt.Errorf("issuer %q audience must be %q", issuer.Issuer, kubernetesMintAudience)
		}
		if !strings.HasPrefix(issuer.JWKSFile, "/") {
			return fmt.Errorf("issuer %q jwks_file must be an absolute path", issuer.Issuer)
		}
		if len(issuer.Namespaces) == 0 {
			return fmt.Errorf("issuer %q must authorize at least one namespace", issuer.Issuer)
		}
		for namespaceName, namespace := range issuer.Namespaces {
			if len(namespaceName) > 63 || !kubernetesNamespacePattern.MatchString(namespaceName) {
				return fmt.Errorf("invalid namespace name %q", namespaceName)
			}
			if strings.TrimSpace(namespace.Tenant) == "" || len(namespace.Roles) == 0 {
				return fmt.Errorf("namespace %q requires tenant and roles", namespaceName)
			}
			for role, policy := range namespace.Roles {
				if err := mintcore.ValidateRoleName(role); err != nil {
					return fmt.Errorf("namespace %q has invalid role %q: %w", namespaceName, role, err)
				}
				if policy.InstallationID <= 0 || len(policy.RepositoryIDs) == 0 || len(policy.AllowedLevels) == 0 {
					return fmt.Errorf("namespace %q role %q requires installation_id, repository_ids, and allowed_levels", namespaceName, role)
				}
				repositoryIDs := map[int64]bool{}
				for _, repositoryID := range policy.RepositoryIDs {
					if repositoryID <= 0 || repositoryIDs[repositoryID] {
						return fmt.Errorf("namespace %q role %q has invalid or duplicate repository ID", namespaceName, role)
					}
					repositoryIDs[repositoryID] = true
				}
				levels := map[string]bool{}
				for _, level := range policy.AllowedLevels {
					if levels[level] {
						return fmt.Errorf("namespace %q role %q repeats level %q", namespaceName, role, level)
					}
					if _, err := mintcore.RolePermissionsForLevel(role, level); err != nil {
						return fmt.Errorf("namespace %q role %q has invalid allowed level %q: %w", namespaceName, role, level, err)
					}
					levels[level] = true
				}
			}
		}
	}
	return nil
}

func (h *kubernetesMintHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMintError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeMintError(w, http.StatusUnauthorized, "authentication failed")
		return
	}
	identity, err := h.authenticate(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		writeMintError(w, http.StatusUnauthorized, "authentication failed")
		return
	}
	defer r.Body.Close()
	var request kubernetesMintRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeMintError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeMintError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if request.RepositoryID != identity.repository || request.Role != identity.role || request.Level == "" {
		writeMintError(w, http.StatusForbidden, "request scope does not match workload identity")
		return
	}
	namespacePolicy := identity.issuer.Namespaces[identity.namespace]
	rolePolicy := namespacePolicy.Roles[identity.role]
	if !containsRepositoryID(rolePolicy.RepositoryIDs, identity.repository) {
		writeMintError(w, http.StatusForbidden, "repository is not authorized for this workload")
		return
	}
	if !containsExact(rolePolicy.AllowedLevels, request.Level) {
		writeMintError(w, http.StatusForbidden, "role or privilege level is not allowed")
		return
	}
	appID := h.appIDs[identity.role]
	if appID == "" {
		writeMintError(w, http.StatusForbidden, "role is not configured")
		return
	}
	pemData, err := h.pemAccessor.AccessPEM(r.Context(), identity.role)
	if err != nil {
		writeMintError(w, http.StatusForbidden, "role credentials are unavailable")
		return
	}
	appJWT, signErr := mintcore.GenerateAppJWT(appID, pemData)
	for i := range pemData {
		pemData[i] = 0
	}
	if signErr != nil {
		writeMintError(w, http.StatusInternalServerError, "could not sign GitHub App assertion")
		return
	}
	token, expiresAt, granted, err := h.exchangeInstallationToken(
		r.Context(), "https://api.github.com", appJWT, rolePolicy.InstallationID,
		identity.role, request.Level, []int64{identity.repository},
	)
	if err != nil {
		writeMintError(w, http.StatusBadGateway, "GitHub installation token exchange failed")
		return
	}
	expectedPermissions, err := mintcore.RolePermissionsForLevel(identity.role, request.Level)
	if err != nil {
		writeMintError(w, http.StatusInternalServerError, "could not resolve GitHub role permissions")
		return
	}
	if granted == nil || len(granted.RepositoryIDs) != 1 || granted.RepositoryIDs[0] != identity.repository ||
		!exactGitHubPermissions(granted.Permissions, expectedPermissions) {
		writeMintError(w, http.StatusBadGateway, "GitHub did not confirm the requested repository and role scope")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(kubernetesMintResponse{
		Token: token, ExpiresAt: expiresAt,
		RepositoryIDs: granted.RepositoryIDs, Permissions: granted.Permissions,
	})
}

func exactGitHubPermissions(granted, expected map[string]string) bool {
	if len(granted) != len(expected) {
		return false
	}
	for permission, level := range expected {
		if granted[permission] != level {
			return false
		}
	}
	return true
}

func (h *kubernetesMintHandler) authenticate(rawToken string) (verifiedKubernetesIdentity, error) {
	var identity verifiedKubernetesIdentity
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return identity, fmt.Errorf("invalid JWT structure")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity, fmt.Errorf("decoding JWT claims: %w", err)
	}
	var claims kubernetesJWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return identity, fmt.Errorf("decoding JWT claims: %w", err)
	}
	issuer, ok := h.issuers[claims.Issuer]
	if !ok {
		return identity, fmt.Errorf("untrusted JWT issuer")
	}
	var header kubernetesJWTHeader
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil {
		return identity, fmt.Errorf("invalid JWT header")
	}
	if header.Algorithm != "RS256" || header.KeyID == "" || (header.Type != "" && header.Type != "JWT") {
		return identity, fmt.Errorf("unsupported JWT header")
	}
	keys, err := loadKubernetesPublicKeys(issuer.JWKSFile)
	if err != nil {
		return identity, fmt.Errorf("loading issuer signing keys: %w", err)
	}
	key := keys[header.KeyID]
	if key == nil {
		return identity, fmt.Errorf("unknown JWT signing key")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity, fmt.Errorf("decoding JWT signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return identity, fmt.Errorf("invalid JWT signature")
	}
	if claims.Issuer != issuer.Issuer || len(claims.Audience) != 1 || !claims.Audience.Contains(issuer.Audience) {
		return identity, fmt.Errorf("JWT issuer or audience mismatch")
	}
	now := time.Now()
	if claims.IssuedAt <= 0 || claims.NotBefore <= 0 || claims.ExpiresAt <= 0 {
		return identity, fmt.Errorf("JWT time claims are required")
	}
	issuedAt := time.Unix(claims.IssuedAt, 0)
	notBefore := time.Unix(claims.NotBefore, 0)
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if issuedAt.After(now.Add(kubernetesTokenClockSkew)) || notBefore.After(now.Add(kubernetesTokenClockSkew)) || !expiresAt.After(now.Add(-kubernetesTokenClockSkew)) || !expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > maxKubernetesTokenAge+kubernetesTokenClockSkew {
		return identity, fmt.Errorf("JWT validity period is not acceptable")
	}
	if claims.Kubernetes.Namespace == "" || claims.Kubernetes.ServiceAccount.Name == "" || claims.Kubernetes.ServiceAccount.UID == "" || claims.Subject != "system:serviceaccount:"+claims.Kubernetes.Namespace+":"+claims.Kubernetes.ServiceAccount.Name {
		return identity, fmt.Errorf("JWT subject and Kubernetes claims are inconsistent")
	}
	match := serviceAccountNamePattern.FindStringSubmatch(claims.Kubernetes.ServiceAccount.Name)
	if len(match) != 3 {
		return identity, fmt.Errorf("service account name does not encode repository and role")
	}
	repositoryID, err := parsePositiveRepositoryID(match[1])
	if err != nil {
		return identity, err
	}
	if _, ok := issuer.Namespaces[claims.Kubernetes.Namespace]; !ok {
		return identity, fmt.Errorf("namespace is not authorized for this issuer")
	}
	return verifiedKubernetesIdentity{
		issuer: issuer, namespace: claims.Kubernetes.Namespace,
		repository: repositoryID, role: match[2],
	}, nil
}

func loadKubernetesPublicKeys(path string) (map[string]*rsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxKubernetesJWKSSize {
		return nil, fmt.Errorf("JWKS size is invalid")
	}
	var set kubernetesJWKSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("decoding JWKS: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range set.Keys {
		if jwk.D != "" || jwk.P != "" || jwk.Q != "" || jwk.DP != "" || jwk.DQ != "" || jwk.QI != "" || (len(jwk.OtherPrimes) > 0 && string(jwk.OtherPrimes) != "null") {
			return nil, fmt.Errorf("JWKS contains private key material")
		}
		if jwk.KeyType != "RSA" || jwk.Alg != "RS256" || jwk.Use != "sig" || jwk.KeyID == "" {
			continue
		}
		if _, exists := keys[jwk.KeyID]; exists {
			return nil, fmt.Errorf("JWKS contains duplicate key ID")
		}
		modulusBytes, err := base64.RawURLEncoding.DecodeString(jwk.Modulus)
		if err != nil || len(modulusBytes) < 256 {
			return nil, fmt.Errorf("JWKS contains invalid RSA modulus")
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(jwk.Exponent)
		if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
			return nil, fmt.Errorf("JWKS contains invalid RSA exponent")
		}
		exponent := 0
		for _, b := range exponentBytes {
			exponent = exponent<<8 | int(b)
		}
		if exponent < 3 || exponent%2 == 0 {
			return nil, fmt.Errorf("JWKS contains invalid RSA exponent")
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulusBytes), E: exponent}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS contains no supported RSA signing keys")
	}
	return keys, nil
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsRepositoryID(values []int64, want int64) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func parsePositiveRepositoryID(raw string) (int64, error) {
	var id int64
	if _, err := fmt.Sscan(raw, &id); err != nil || id <= 0 || fmt.Sprint(id) != raw {
		return 0, fmt.Errorf("service account contains invalid repository ID")
	}
	return id, nil
}

func writeMintError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
