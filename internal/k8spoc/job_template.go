package k8spoc

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var jobNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var imageDigestPattern = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
var localRunnerTagPattern = regexp.MustCompile(`^localhost/fullsend-poc-runner:[0-9a-f]{12}-[0-9a-f]{16}$`)

func validProbeName(name string) bool {
	return len(name) >= 4 && len(name) <= 17 && len(name) >= 3 && name[:3] == "fs-" && jobNamePattern.MatchString(name)
}

// jobTemplate is shared by the dispatcher and the manual gateway probe.
// Callers supply only the container and its explicitly scoped volumes.
func jobTemplate(name, namespace string, attemptID int64, container map[string]any, volumes []any) map[string]any {
	pod := map[string]any{
		"serviceAccountName": "runner", "automountServiceAccountToken": false,
		"restartPolicy":   "Never",
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001, "fsGroup": 10001, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"containers":      []any{container},
	}
	if len(volumes) > 0 {
		pod["volumes"] = volumes
	}
	labels := map[string]string{"app.kubernetes.io/part-of": "fullsend-kind-poc"}
	if attemptID > 0 {
		labels["fullsend.ai/attempt"] = fmt.Sprint(attemptID)
	}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": labels},
		"spec": map[string]any{"backoffLimit": 0, "activeDeadlineSeconds": 600,
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/part-of": "fullsend-kind-poc"}}, "spec": pod}},
	}
}

func restrictedContainer(name, image string, command []string, cpuRequest, memoryRequest, cpuLimit, memoryLimit string) map[string]any {
	return map[string]any{
		"name": name, "image": image, "imagePullPolicy": "IfNotPresent", "command": command,
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
		"resources":       map[string]any{"requests": map[string]string{"cpu": cpuRequest, "memory": memoryRequest}, "limits": map[string]string{"cpu": cpuLimit, "memory": memoryLimit}},
	}
}

func stubJob(a attempt, namespace, image string) map[string]any {
	args := []string{"sh", "-c", "echo fixture-run-complete"}
	switch a.behavior {
	case "failure":
		args = []string{"sh", "-c", "echo fixture-run-failed >&2; exit 23"}
	case "active":
		args = []string{"sh", "-c", "sleep 90"}
	}
	job := jobTemplate(a.name, namespace, a.id, restrictedContainer("stub", image, args, "10m", "16Mi", "100m", "64Mi"), nil)
	job["spec"].(map[string]any)["activeDeadlineSeconds"] = 180
	return job
}

// GatewayProbeJob renders the dispatcher Job template with a persistent
// evidence volume and a gateway bundle Secret. The Secret contains only the
// OpenShell client connection bundle, not signing or forge credentials.
func GatewayProbeJob(name, probeName, runnerImage, sandboxImage string) ([]byte, error) {
	if len(name) > 63 || !jobNamePattern.MatchString(name) || !validProbeName(probeName) || len(runnerImage) == 0 || len(sandboxImage) == 0 {
		return nil, fmt.Errorf("valid Job name, probe name, runner image and sandbox image are required")
	}
	if !containsDigest(sandboxImage) {
		return nil, fmt.Errorf("sandbox image must be pinned by sha256 digest")
	}
	return gatewayRunnerJob(name, runnerImage, "probe", []string{"/usr/local/bin/fullsend", "k8s-poc", "gateway-probe", "--name", probeName, "--sandbox-image", sandboxImage, "--output-dir", "/evidence/" + name})
}

// GatewayDemoJob runs the pinned local harness through `fullsend run` using
// the same restricted runner Pod shape as the dispatcher and transport probe.
func GatewayDemoJob(name, runnerImage string) ([]byte, error) {
	names, ok := ownedGatewayNames(name)
	if !ok || len(names) != 1 {
		return nil, fmt.Errorf("demo Job name must use fs-demo-<eight lowercase hex digits>")
	}
	jobBytes, err := gatewayRunnerJob(name, runnerImage, "demo", []string{
		"/usr/local/bin/fullsend", "k8s-poc", "gateway-demo",
		"--job-name", name, "--sandbox-name", names[0],
		"--demo-dir", "/demo", "--evidence-dir", "/evidence/" + name,
	})
	if err != nil {
		return nil, err
	}
	var job map[string]any
	if err := json.Unmarshal(jobBytes, &job); err != nil {
		return nil, err
	}
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	volumes := pod["volumes"].([]any)
	pod["volumes"] = append(volumes,
		map[string]any{"name": "demo-bundle", "configMap": map[string]any{"name": "fullsend-gateway-demo"}},
		map[string]any{"name": "demo", "emptyDir": map[string]any{}},
	)
	initContainers := pod["initContainers"].([]any)
	init := restrictedContainer("demo-config", runnerImage,
		[]string{"sh", "-ec", "tar --no-same-owner --no-same-permissions --touch -xzf /demo-bundle/bundle.tgz -C /demo && git -C /demo/repo init -q && git -C /demo/repo add sample.txt && git -C /demo/repo -c user.name=fullsend-demo -c user.email=demo@example.invalid commit -qm 'Add demo input'"},
		"10m", "32Mi", "100m", "128Mi")
	init["volumeMounts"] = []any{
		map[string]any{"name": "demo-bundle", "mountPath": "/demo-bundle", "readOnly": true},
		map[string]any{"name": "demo", "mountPath": "/demo"},
	}
	pod["initContainers"] = append(initContainers, init)
	container := pod["containers"].([]any)[0].(map[string]any)
	container["volumeMounts"] = append(container["volumeMounts"].([]any), map[string]any{"name": "demo", "mountPath": "/demo"})
	return json.MarshalIndent(job, "", "  ")
}

// GatewayCleanupJob uses the same runner shape to reconcile probe sandboxes
// after a runner was killed, before the owned kind cluster is destroyed.
func GatewayCleanupJob(name, runnerImage string) ([]byte, error) {
	if !probeJobPattern.MatchString(name) {
		return nil, fmt.Errorf("cleanup Job name must use an owned fs-gateway-<id> name")
	}
	return gatewayRunnerJob(name, runnerImage, "cleanup", []string{"/usr/local/bin/fullsend", "k8s-poc", "cleanup-gateway-probes", "--evidence-dir", "/evidence"})
}

func gatewayRunnerJob(name, runnerImage, purpose string, command []string) ([]byte, error) {
	if len(name) > 63 || !jobNamePattern.MatchString(name) {
		return nil, fmt.Errorf("valid Job name is required")
	}
	if !containsDigest(runnerImage) && !localRunnerTagPattern.MatchString(runnerImage) {
		return nil, fmt.Errorf("runner image must use a sha256 digest or a local revision-and-content tag")
	}
	// !!! TEMPORARY TLS VERIFICATION BYPASS !!!
	// !!! Managed gateway CA and public-hostname certificate issue:
	// !!! https://github.com/openshift-online/hypershell/issues/443
	// !!! Remove this warning and OPENSHELL_GATEWAY_INSECURE together.
	// The pinned CLI exchanges the service-account secret on gateway login;
	// subsequent fullsend subprocesses read the resulting short-lived token.
	warning := "printf '%s\\n' '!!! OPENSHELL_GATEWAY_INSECURE=true: gateway TLS certificate verification is DISABLED for this PoC !!!' '!!! Track the CA and public-hostname fix: https://github.com/openshift-online/hypershell/issues/443 !!!' >&2; openshell gateway login; exec \"$@\""
	runnerCommand := append([]string{"sh", "-ec", warning, "gateway-tls-warning"}, command...)
	container := restrictedContainer("runner", runnerImage, runnerCommand, "100m", "256Mi", "2", "2Gi")
	container["env"] = []any{
		map[string]any{"name": "XDG_CONFIG_HOME", "value": "/config"},
		map[string]any{"name": "HOME", "value": "/home/runner"},
		map[string]any{"name": "OPENSHELL_GATEWAY_INSECURE", "value": "true"},
		map[string]any{"name": "OPENSHELL_OIDC_CLIENT_SECRET", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "fullsend-gateway-auth", "key": "client-secret"}}},
	}
	container["volumeMounts"] = []any{
		map[string]any{"name": "gateway-config", "mountPath": "/config"},
		map[string]any{"name": "evidence", "mountPath": "/evidence"},
		map[string]any{"name": "home", "mountPath": "/home/runner"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
	}
	volumes := []any{
		map[string]any{"name": "gateway-bundle", "secret": map[string]any{"secretName": "fullsend-gateway-bundle"}},
		map[string]any{"name": "gateway-config", "emptyDir": map[string]any{}},
		map[string]any{"name": "evidence", "persistentVolumeClaim": map[string]any{"claimName": "fullsend-runner-evidence"}},
		map[string]any{"name": "home", "emptyDir": map[string]any{}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
	}
	job := jobTemplate(name, "fullsend-runner", 0, container, volumes)
	job["metadata"].(map[string]any)["labels"].(map[string]string)["fullsend.ai/gateway-job"] = purpose
	spec := job["spec"].(map[string]any)
	spec["activeDeadlineSeconds"] = 1200
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	pod["initContainers"] = []any{map[string]any{
		"name": "gateway-config", "image": runnerImage, "imagePullPolicy": "IfNotPresent",
		"command":         []string{"sh", "-ec", "mkdir -p /config/openshell && tar -xzf /gateway/bundle.tgz -C /config/openshell && chmod -R go-rwx /config/openshell"},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
		"resources":       map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]string{"cpu": "100m", "memory": "128Mi"}},
		"volumeMounts":    []any{map[string]any{"name": "gateway-bundle", "mountPath": "/gateway", "readOnly": true}, map[string]any{"name": "gateway-config", "mountPath": "/config"}},
	}}
	return json.MarshalIndent(job, "", "  ")
}

func containsDigest(image string) bool {
	return imageDigestPattern.MatchString(image)
}
