package k8spoc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGatewayProbeJobIsolation(t *testing.T) {
	image := "localhost/fullsend-poc-runner@sha256:" + strings.Repeat("a", 64)
	sandboxImage := "ghcr.io/example/sandbox@sha256:" + strings.Repeat("b", 64)
	data, err := GatewayProbeJob("fs-probe-123", "fs-12345678", image, sandboxImage)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false || pod["serviceAccountName"] != "runner" {
		t.Fatalf("unexpected pod identity: %v", pod)
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	if container["image"] != image {
		t.Fatalf("runner image = %v", container["image"])
	}
	assertGatewayInsecureWarning(t, container)
	volumes := pod["volumes"].([]any)
	if len(volumes) != 5 {
		t.Fatalf("volumes = %v", volumes)
	}
	if !strings.Contains(string(data), `"claimName": "fullsend-runner-evidence"`) || !strings.Contains(string(data), `"secretName": "fullsend-gateway-bundle"`) {
		t.Fatalf("missing evidence or gateway volume: %s", data)
	}
	if strings.Contains(string(data), "postgres-auth") || strings.Contains(string(data), "jira-token") {
		t.Fatalf("probe Job received unrelated credentials: %s", data)
	}
	if !strings.Contains(string(data), `"name": "fullsend-gateway-auth"`) || strings.Contains(string(data), "oidc_token.json") {
		t.Fatalf("probe Job must use service-account auth Secret only: %s", data)
	}
}

func TestGatewayProbeJobRequiresPinnedImages(t *testing.T) {
	_, err := GatewayProbeJob("fs-probe-123", "fs-12345678", "localhost/runner:latest", "ghcr.io/sandbox:latest")
	if err == nil {
		t.Fatal("expected mutable image references to be rejected")
	}
	_, err = GatewayProbeJob("fs-probe-123", "not-owned", "image@sha256:"+strings.Repeat("a", 64), "image@sha256:"+strings.Repeat("b", 64))
	if err == nil {
		t.Fatal("expected invalid probe name to be rejected")
	}
}

func TestGatewayDemoJobUsesRestrictedRunnerAndPinnedBundle(t *testing.T) {
	image := "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef"
	data, err := GatewayDemoJob("fs-demo-deadbeef", image)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false || pod["serviceAccountName"] != "runner" {
		t.Fatalf("unexpected pod identity: %v", pod)
	}
	if !strings.Contains(string(data), `"name": "fullsend-gateway-demo"`) || !strings.Contains(string(data), "gateway-demo") {
		t.Fatalf("demo bundle or command missing: %s", data)
	}
	if strings.Contains(string(data), "postgres-auth") || strings.Contains(string(data), "jira-live-auth") {
		t.Fatalf("demo Job received source/control credentials: %s", data)
	}
	if _, err := GatewayDemoJob("other", image); err == nil {
		t.Fatal("accepted foreign demo Job name")
	}
}

func TestGatewayCleanupJobUsesRunnerTemplate(t *testing.T) {
	image := "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef"
	data, err := GatewayCleanupJob("fs-gateway-deadbeef", image)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	labels := job["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["fullsend.ai/gateway-job"] != "cleanup" {
		t.Fatalf("cleanup label missing: %v", labels)
	}
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false {
		t.Fatal("cleanup Job mounted Kubernetes token")
	}
	assertGatewayInsecureWarning(t, pod["containers"].([]any)[0].(map[string]any))
	if !strings.Contains(string(data), "cleanup-gateway-probes") {
		t.Fatal("cleanup command missing")
	}
	if _, err := GatewayCleanupJob("other", image); err == nil {
		t.Fatal("accepted non-PoC Job name")
	}
}

func TestGatewayAttemptJobsBindDurableIdentity(t *testing.T) {
	image := "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef"
	name := "fs-poc-23-2"
	sandboxName, err := attemptSandboxName(name)
	if err != nil {
		t.Fatal(err)
	}
	if sandboxName == "" || sandboxName == "fs-poc-23-2" {
		t.Fatalf("unexpected sandbox name %q", sandboxName)
	}
	data, err := GatewayAttemptJob(name, image, 17)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	labels := job["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["fullsend.ai/attempt"] != "17" || labels["fullsend.ai/gateway-job"] != "demo" {
		t.Fatalf("incorrect attempt labels: %v", labels)
	}
	if !strings.Contains(string(data), sandboxName) || !strings.Contains(string(data), "/evidence/"+name) {
		t.Fatalf("runner did not receive durable identity and evidence path: %s", data)
	}
	cleanup, err := GatewayAttemptCleanupJob(name, image, 17)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cleanup), "fs-clean-23-2") || !strings.Contains(string(cleanup), "cleanup-gateway-attempt") {
		t.Fatalf("cleanup Job did not target attempt: %s", cleanup)
	}
	var cleanupJob map[string]any
	if err := json.Unmarshal(cleanup, &cleanupJob); err != nil {
		t.Fatal(err)
	}
	if cleanupJob["spec"].(map[string]any)["backoffLimit"] != float64(3) {
		t.Fatal("cleanup Job must retry transient gateway failures")
	}
	for _, invalid := range []string{"fs-poc-0-1", "fs-poc-1-0", "fs-demo-deadbeef", "fs-poc-1-1/foreign"} {
		if _, err := GatewayAttemptJob(invalid, image, 17); err == nil {
			t.Fatalf("accepted invalid attempt name %q", invalid)
		}
	}
	if _, err := GatewayAttemptCleanupJob(name, image, 0); err == nil {
		t.Fatal("accepted zero attempt ID")
	}
}

func TestGatewayAttemptMintJobScopesProjectedIdentity(t *testing.T) {
	image := "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef"
	data, err := GatewayAttemptMintJob("fs-poc-23-2", image, 17, 123456789, gatewayDemoRole, kubernetesMintServiceURL)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["serviceAccountName"] != "fs-repo-123456789-triage" || pod["automountServiceAccountToken"] != false {
		t.Fatalf("unexpected projected identity: %v", pod)
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	env := map[string]string{}
	for _, entry := range container["env"].([]any) {
		item := entry.(map[string]any)
		if value, ok := item["value"].(string); ok {
			env[item["name"].(string)] = value
		}
	}
	for key, want := range map[string]string{
		"FULLSEND_MINT_URL":          kubernetesMintServiceURL,
		"FULLSEND_REPOSITORY_ID":     "123456789",
		"KUBERNETES_MINT_TOKEN_FILE": kubernetesMintTokenPath,
		"KUBERNETES_MINT_CA_FILE":    kubernetesMintCAPath,
	} {
		if env[key] != want {
			t.Fatalf("env %s = %q, want %q", key, env[key], want)
		}
	}
	volumes := pod["volumes"].([]any)
	var mintVolume map[string]any
	for _, entry := range volumes {
		volume := entry.(map[string]any)
		if volume["name"] == "mint-identity" {
			mintVolume = volume
		}
	}
	if mintVolume == nil {
		t.Fatalf("mint identity volume missing: %v", volumes)
	}
	sources := mintVolume["projected"].(map[string]any)["sources"].([]any)
	token := sources[0].(map[string]any)["serviceAccountToken"].(map[string]any)
	if token["audience"] != kubernetesMintAudience || token["expirationSeconds"] != float64(600) {
		t.Fatalf("unexpected projected token: %v", token)
	}
	ca := sources[1].(map[string]any)["configMap"].(map[string]any)
	if ca["name"] != kubernetesMintServiceCAName {
		t.Fatalf("unexpected mint CA source: %v", ca)
	}
	if strings.Contains(string(data), "fullsend-app-pem") || strings.Contains(string(data), "ROLE_APP_IDS") {
		t.Fatal("dispatcher runner Job must not receive App signing key material")
	}
	for _, tc := range []struct {
		name          string
		repositoryID  int64
		role, mintURL string
	}{
		{name: "zero repository", repositoryID: 0, role: gatewayDemoRole, mintURL: kubernetesMintServiceURL},
		{name: "invalid role", repositoryID: 123456789, role: "not a role", mintURL: kubernetesMintServiceURL},
		{name: "non-HTTPS mint", repositoryID: 123456789, role: gatewayDemoRole, mintURL: "http://fullsend-mint.fullsend-mint.svc"},
		{name: "external mint", repositoryID: 123456789, role: gatewayDemoRole, mintURL: "https://mint.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := GatewayAttemptMintJob("fs-poc-23-2", image, 17, tc.repositoryID, tc.role, tc.mintURL); err == nil {
				t.Fatal("accepted invalid mint job identity")
			}
		})
	}
}

func assertGatewayInsecureWarning(t *testing.T, container map[string]any) {
	t.Helper()
	found := false
	for _, entry := range container["env"].([]any) {
		env := entry.(map[string]any)
		if env["name"] == "OPENSHELL_GATEWAY_INSECURE" && env["value"] == "true" {
			found = true
		}
	}
	if !found {
		t.Fatal("runner does not set OPENSHELL_GATEWAY_INSECURE=true")
	}
	command := container["command"].([]any)
	if command[0] != "sh" || !strings.Contains(command[2].(string), "https://github.com/openshift-online/hypershell/issues/443") || !strings.Contains(command[2].(string), "openshell gateway login") {
		t.Fatalf("runner does not warn and authenticate before execution: %v", command)
	}
}
