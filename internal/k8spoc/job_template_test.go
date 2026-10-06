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
