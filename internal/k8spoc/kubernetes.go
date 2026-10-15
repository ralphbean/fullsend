package k8spoc

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type kubeClient struct {
	client                                      *http.Client
	host, token, namespace, image, gatewayImage string
	mintRepositoryID                            int64
}

func newKubeClient() (*kubeClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST is required")
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = "443"
	}
	k, err := newKubeClientWithFiles(host, port, os.Getenv("STUB_IMAGE"), "/var/run/secrets/kubernetes.io/serviceaccount/token", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, err
	}
	k.gatewayImage = os.Getenv("GATEWAY_RUNNER_IMAGE")
	if err := configureMintRepositoryID(k, os.Getenv("FULLSEND_REPOSITORY_ID")); err != nil {
		return nil, err
	}
	return k, nil
}

func configureMintRepositoryID(k *kubeClient, raw string) error {
	if raw == "" {
		return nil
	}
	repositoryID, err := parsePositiveRepositoryID(raw)
	if err != nil {
		return fmt.Errorf("invalid FULLSEND_REPOSITORY_ID for dispatcher mint: %w", err)
	}
	k.mintRepositoryID = repositoryID
	return nil
}

func newKubeClientWithFiles(host, port, image, tokenPath, caPath string) (*kubeClient, error) {
	if image == "" {
		return nil, fmt.Errorf("STUB_IMAGE is required")
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes CA")
	}
	return &kubeClient{client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, host: "https://" + host + ":" + port, token: string(token), namespace: "fullsend-runner", image: image}, nil
}

func parsePositiveRepositoryID(raw string) (int64, error) {
	repositoryID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || repositoryID <= 0 || strconv.FormatInt(repositoryID, 10) != raw {
		return 0, fmt.Errorf("must be a canonical positive integer")
	}
	return repositoryID, nil
}

type kubeJob struct {
	Metadata struct {
		Name   string            `json:"name"`
		UID    string            `json:"uid"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Active     int                `json:"active"`
		Succeeded  int                `json:"succeeded"`
		Failed     int                `json:"failed"`
		Conditions []kubeJobCondition `json:"conditions"`
	} `json:"status"`
}

type kubeJobCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

func jobFailed(job kubeJob) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == "Failed" && condition.Status == "True" {
			return true
		}
	}
	return false
}

func (k *kubeClient) request(ctx context.Context, method, path string, body any, output any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.host+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return resp.StatusCode, fmt.Errorf("kubernetes %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if output != nil {
		if err = json.NewDecoder(resp.Body).Decode(output); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (k *kubeClient) jobPath(name string) string {
	return "/apis/batch/v1/namespaces/" + k.namespace + "/jobs/" + name
}

func (k *kubeClient) get(ctx context.Context, name string) (kubeJob, bool, error) {
	var job kubeJob
	status, err := k.request(ctx, "GET", k.jobPath(name), nil, &job)
	if status == 404 {
		return job, false, nil
	}
	return job, true, err
}

func (k *kubeClient) create(ctx context.Context, a attempt) (kubeJob, error) {
	var job kubeJob
	var body any = stubJob(a, k.namespace, k.image)
	if isGatewayAttempt(a) {
		var data []byte
		var err error
		if a.execution == "fullsend" {
			if k.mintRepositoryID <= 0 {
				return job, fmt.Errorf("real Jira triage requires its registered repository mint identity")
			}
			data, err = GatewayJiraTriageAttemptJob(a, k.gatewayImage, k.mintRepositoryID)
			if err == nil {
				err = k.ensureJiraEventConfigMap(ctx, a)
			}
			if err == nil {
				err = k.ensureMintServiceAccount(ctx, k.mintRepositoryID, gatewayDemoRole)
			}
		} else if k.mintRepositoryID > 0 {
			data, err = GatewayAttemptMintJob(a.name, k.gatewayImage, a.id, k.mintRepositoryID, gatewayDemoRole, kubernetesMintServiceURL)
		} else {
			data, err = GatewayAttemptJob(a.name, k.gatewayImage, a.id)
		}
		if err != nil {
			return job, err
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return job, err
		}
		if k.mintRepositoryID > 0 {
			if err := k.ensureMintServiceAccount(ctx, k.mintRepositoryID, gatewayDemoRole); err != nil {
				return job, err
			}
		}
	}
	_, err := k.request(ctx, "POST", "/apis/batch/v1/namespaces/"+k.namespace+"/jobs", body, &job)
	return job, err
}

func (k *kubeClient) ensureJiraEventConfigMap(ctx context.Context, a attempt) error {
	if a.runID < 1 || !json.Valid(a.eventPayload) || len(a.eventPayload) > 16*1024 {
		return fmt.Errorf("invalid durable Jira event for attempt %d", a.id)
	}
	name := fmt.Sprintf("fullsend-event-%d", a.runID)
	path := "/api/v1/namespaces/" + k.namespace + "/configmaps"
	body := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":  map[string]any{"name": name, "namespace": k.namespace, "labels": map[string]string{"app.kubernetes.io/part-of": "fullsend-kind-poc", "fullsend.ai/run": fmt.Sprint(a.runID)}},
		"data":      map[string]string{"event.json": string(a.eventPayload)},
		"immutable": true,
	}
	_, err := k.request(ctx, "POST", path, body, nil)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), " 409 ") {
		return err
	}
	var existing struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	status, getErr := k.request(ctx, "GET", path+"/"+name, nil, &existing)
	if getErr != nil || status != http.StatusOK || existing.Metadata.Name != name || existing.Data["event.json"] != string(a.eventPayload) {
		return fmt.Errorf("existing Jira event ConfigMap %s conflicts with the selected run", name)
	}
	return nil
}

func (k *kubeClient) deleteJiraEventConfigMap(ctx context.Context, runID int64) error {
	if runID < 1 {
		return fmt.Errorf("run ID must be positive")
	}
	_, err := k.request(ctx, "DELETE", "/api/v1/namespaces/"+k.namespace+"/configmaps/"+fmt.Sprintf("fullsend-event-%d", runID), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions"}, nil)
	if err != nil && strings.Contains(err.Error(), " 404 ") {
		return nil
	}
	return err
}

type kubeServiceAccount struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken"`
}

func (k *kubeClient) ensureMintServiceAccount(ctx context.Context, repositoryID int64, role string) error {
	if repositoryID <= 0 || role != gatewayDemoRole {
		return fmt.Errorf("dispatcher mint identity is not authorized")
	}
	name := fmt.Sprintf("fs-repo-%d-%s", repositoryID, role)
	path := "/api/v1/namespaces/" + k.namespace + "/serviceaccounts/" + name
	var current kubeServiceAccount
	status, err := k.request(ctx, "GET", path, nil, &current)
	if err != nil && status != http.StatusNotFound {
		return err
	}
	if status == http.StatusOK {
		return validateMintServiceAccount(current, name, repositoryID, role)
	}
	disabled := false
	body := map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata": map[string]any{
			"name":      name,
			"namespace": k.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/part-of": "fullsend-kind-poc",
				"fullsend.ai/repository-id": fmt.Sprint(repositoryID),
				"fullsend.ai/role":          role,
			},
			"annotations": map[string]string{"fullsend.ai/mint-identity": "true"},
		},
		"automountServiceAccountToken": disabled,
	}
	if status, err = k.request(ctx, "POST", "/api/v1/namespaces/"+k.namespace+"/serviceaccounts", body, nil); err == nil {
		return nil
	} else if status != http.StatusConflict {
		return err
	}
	// Concurrent dispatchers may both observe absence. A create conflict is
	// safe only when the winning object has exactly the expected identity.
	status, err = k.request(ctx, "GET", path, nil, &current)
	if err != nil || status != http.StatusOK {
		if err != nil {
			return err
		}
		return fmt.Errorf("mint service account %s disappeared after create conflict", name)
	}
	return validateMintServiceAccount(current, name, repositoryID, role)
}

func validateMintServiceAccount(account kubeServiceAccount, name string, repositoryID int64, role string) error {
	if account.Metadata.Name != name || account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken ||
		account.Metadata.Labels["fullsend.ai/repository-id"] != fmt.Sprint(repositoryID) ||
		account.Metadata.Labels["fullsend.ai/role"] != role ||
		account.Metadata.Annotations["fullsend.ai/mint-identity"] != "true" {
		return fmt.Errorf("existing mint service account %s conflicts with the approved repository/role identity", name)
	}
	return nil
}

func (k *kubeClient) createGatewayCleanup(ctx context.Context, a attempt) (kubeJob, error) {
	var job kubeJob
	data, err := GatewayAttemptCleanupJob(a.name, k.gatewayImage, a.id)
	if err != nil {
		return job, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return job, err
	}
	_, err = k.request(ctx, "POST", "/apis/batch/v1/namespaces/"+k.namespace+"/jobs", body, &job)
	return job, err
}

func isGatewayAttempt(a attempt) bool {
	return (a.harness == "gateway-demo" && a.source == "fixture-source") || (a.harness == "triage" && a.execution == "fullsend" && strings.HasPrefix(a.source, "jira:"))
}

func cleanupJobName(name string) string {
	return "fs-clean-" + strings.TrimPrefix(name, "fs-poc-")
}

func (k *kubeClient) delete(ctx context.Context, name, uid string) error {
	_, err := k.request(ctx, "DELETE", k.jobPath(name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground", "preconditions": map[string]string{"uid": uid}}, nil)
	if err != nil && strings.Contains(err.Error(), " 404 ") {
		return nil
	}
	return err
}
