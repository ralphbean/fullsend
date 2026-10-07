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
