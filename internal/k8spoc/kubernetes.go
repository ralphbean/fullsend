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
	"strings"
	"time"
)

type kubeClient struct {
	client                                      *http.Client
	host, token, namespace, image, gatewayImage string
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
	return k, nil
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
		data, err := GatewayAttemptJob(a.name, k.gatewayImage, a.id)
		if err != nil {
			return job, err
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return job, err
		}
	}
	_, err := k.request(ctx, "POST", "/apis/batch/v1/namespaces/"+k.namespace+"/jobs", body, &job)
	return job, err
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
	return a.harness == "gateway-demo" && a.source == "fixture-source"
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
