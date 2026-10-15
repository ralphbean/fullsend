package k8spoc

import (
	"encoding/json"
	"strings"
	"testing"
)

const validTestJiraEventPayload = `{"repo":"owner/repo","entity":{"kind":"work_item","id":123,"key":"FSENDAI-123","url":"https://stage.example/browse/FSENDAI-123"},"transition":{"kind":"opened"},"actor":{"id":"test-account","kind":"human","role":"admin"},"state":{"labels":["fullsend-dispatcher-test"]},"source":{"system":"jira","raw_type":"issue","raw_action":"created"}}`

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
	var demoConfig map[string]any
	for _, raw := range pod["initContainers"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["name"] == "demo-config" {
			demoConfig = candidate
		}
	}
	if demoConfig == nil {
		t.Fatal("demo setup init container is missing")
	}
	demoCommand, _ := json.Marshal(demoConfig["command"])
	if !strings.Contains(string(demoCommand), "--no-overwrite-dir") {
		t.Fatalf("demo extraction may not rewrite the mounted root directory mode: %s", demoCommand)
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

func TestGatewayJiraTriageAttemptUsesPinnedEventAndInferenceIdentity(t *testing.T) {
	image := "localhost/fullsend-poc-runner:abcdef123456-0123456789abcdef"
	configBytes, err := json.Marshal(LiveConfig{
		JiraBaseURL: "https://stage.example", Project: "FSENDAI", Label: "fullsend-dispatcher-test", Repo: "owner/repo",
		Harnesses:         []LiveHarness{{Name: "triage", Role: "triage", Trigger: `event.transition.kind == "opened"`}},
		FullsendHarnesses: []string{"triage"}, RepoBundle: "fullsend-live-repo-abcdef0123456789", RepoRevision: strings.Repeat("a", 40),
		GCPAudience: "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/pool/providers/provider", VertexProject: "vertex-project",
	})
	a := attempt{
		id: 19, runID: 31, name: "fs-poc-31-1", harness: "triage", execution: "fullsend",
		source: "jira:FSENDAI:fullsend-dispatcher-test", eventPayload: []byte(validTestJiraEventPayload),
		configContent: configBytes,
	}
	data, err := GatewayJiraTriageAttemptJob(a, image, 123456789)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	labels := job["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["fullsend.ai/attempt"] != "19" || labels["fullsend.ai/gateway-job"] != "demo" {
		t.Fatalf("real triage Job has incorrect durable attempt labels: %v", labels)
	}
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["serviceAccountName"] != "fs-repo-123456789-triage" || pod["automountServiceAccountToken"] != false {
		t.Fatalf("unexpected inference runner identity: %v", pod)
	}
	var repoConfig map[string]any
	for _, raw := range pod["initContainers"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["name"] == "repo-config" {
			repoConfig = candidate
		}
	}
	if repoConfig == nil {
		t.Fatal("repository setup init container is missing")
	}
	repoCommand, _ := json.Marshal(repoConfig["command"])
	if !strings.Contains(string(repoCommand), "--no-overwrite-dir") || !strings.Contains(string(repoCommand), "safe.directory=/repo") {
		t.Fatalf("repository setup must preserve the mounted root directory mode and trust only its fixed Git directory: %s", repoCommand)
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	var workItemURL string
	for _, raw := range container["env"].([]any) {
		entry := raw.(map[string]any)
		if entry["name"] == "FULLSEND_WORK_ITEM_URL" {
			workItemURL, _ = entry["value"].(string)
		}
	}
	if workItemURL != "https://stage.example/browse/FSENDAI-123" {
		t.Fatalf("runner work item URL = %q", workItemURL)
	}
	command, _ := json.Marshal(container["command"])
	if !strings.Contains(string(command), "triage") || !strings.Contains(string(command), "event.json") || !strings.Contains(string(command), "--keep-sandbox") {
		t.Fatalf("runner command does not invoke the real harness with the event: %s", command)
	}
	jobText := string(data)
	for _, required := range []string{"fullsend-live-repo-abcdef0123456789", "fullsend-event-31", "fs-repo-123456789-triage", "fullsend-k8s-mint", "JIRA_TOKEN", "run-live-fullsend.sh", "triage"} {
		if !strings.Contains(jobText, required) {
			t.Fatalf("real triage Job is missing %q: %s", required, jobText)
		}
	}
	if strings.Contains(jobText, "selected Jira body") || !strings.Contains(jobText, "event.json") {
		t.Fatalf("event content must be mounted from the durable selected run: %s", jobText)
	}
	if _, err := GatewayJiraTriageAttemptJob(attempt{id: 19, runID: 31, name: "fs-poc-31-1", harness: "triage", execution: "fullsend", source: "fixture-source", configContent: a.configContent, eventPayload: []byte("{}")}, image, 123456789); err == nil {
		t.Fatal("accepted non-Jira source")
	}
	for name, payload := range map[string][]byte{
		"invalid normalized event": []byte(`{"source":{"system":"jira"}}`),
		"mismatched Jira URL":      []byte(strings.Replace(validTestJiraEventPayload, "https://stage.example", "https://other.example", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			invalid := a
			invalid.eventPayload = payload
			if _, err := GatewayJiraTriageAttemptJob(invalid, image, 123456789); err == nil {
				t.Fatal("accepted invalid normalized Jira event")
			}
		})
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
