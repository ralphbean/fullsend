package k8spoc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeProbeClient struct {
	files           map[string]map[string][]byte
	created         []string
	deleted         []string
	failExec        bool
	failCreate      bool
	failUpload      bool
	failDelete      bool
	corruptDownload bool
	failAvailable   bool
	failGateway     bool
}

var testProbeImage = "image@sha256:" + strings.Repeat("a", 64)

func (f *fakeProbeClient) available() error {
	if f.failAvailable {
		return errors.New("CLI missing")
	}
	return nil
}

func (f *fakeProbeClient) checkGateway() error {
	if f.failGateway {
		return errors.New("gateway unavailable")
	}
	return nil
}

func (f *fakeProbeClient) create(name, _ string) error {
	if f.files == nil {
		f.files = make(map[string]map[string][]byte)
	}
	f.files[name] = make(map[string][]byte)
	f.created = append(f.created, name)
	if f.failCreate {
		return errors.New("create response lost")
	}
	return nil
}

func (f *fakeProbeClient) upload(name, localPath, remotePath string) error {
	if f.failUpload {
		return errors.New("upload failed")
	}
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	f.files[name][remotePath] = b
	return nil
}

func (f *fakeProbeClient) exec(_ context.Context, name, _ string) (string, int, error) {
	if f.failExec {
		return "simulated failure", 23, nil
	}
	f.files[name]["/sandbox/workspace/probe.output"] = f.files[name]["/sandbox/workspace/probe.input"]
	return "", 0, nil
}

func (f *fakeProbeClient) download(name, remotePath, localPath string) error {
	if f.corruptDownload {
		return os.WriteFile(localPath, []byte("wrong bytes"), 0o600)
	}
	return os.WriteFile(localPath, f.files[name][remotePath], 0o600)
}

func (f *fakeProbeClient) delete(name string) error {
	f.deleted = append(f.deleted, name)
	if f.failDelete {
		return errors.New("remote delete failed")
	}
	delete(f.files, name)
	return nil
}

func TestGatewayProbeTransfersTwoIsolatedWorkspaces(t *testing.T) {
	dir := t.TempDir()
	client := &fakeProbeClient{}
	if err := gatewayProbeWithClient(context.Background(), "fs-probe", "sandbox@sha256:"+strings.Repeat("a", 64), dir, client); err != nil {
		t.Fatal(err)
	}
	if len(client.created) != 2 || len(client.deleted) != 2 || client.created[0] == client.created[1] {
		t.Fatalf("created=%v deleted=%v", client.created, client.deleted)
	}
	intent := readProbeFile(t, dir, "intent.json")
	if len(intent.Names) != 2 || len(intent.Attempted) != 0 {
		t.Fatalf("intent was not saved before creation: %+v", intent)
	}
	result := readProbeFile(t, dir, "result.json")
	if len(result.Transfers) != 2 || len(result.Deleted) != 2 || result.Error != "" {
		t.Fatalf("incomplete result: %+v", result)
	}
	first, err := os.ReadFile(filepath.Join(dir, client.created[0]+".output"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, client.created[1]+".output"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("workspaces produced identical contents")
	}
}

func TestGatewayProbeFailureStillCleansRemoteWorkspaces(t *testing.T) {
	dir := t.TempDir()
	client := &fakeProbeClient{failExec: true}
	err := gatewayProbeWithClient(context.Background(), "fs-probe", testProbeImage, dir, client)
	if err == nil || !strings.Contains(err.Error(), "exit 23") {
		t.Fatalf("expected remote exit status, got %v", err)
	}
	if len(client.deleted) != 2 {
		t.Fatalf("remote workspaces not cleaned: %v", client.deleted)
	}
	result := readProbeFile(t, dir, "result.json")
	if !strings.Contains(result.Error, "exit 23") || len(result.Deleted) != 2 {
		t.Fatalf("failure evidence incomplete: %+v", result)
	}
}

func TestGatewayProbeAmbiguousCreateKeepsIntentAndAttemptsCleanup(t *testing.T) {
	dir := t.TempDir()
	client := &fakeProbeClient{failCreate: true}
	err := gatewayProbeWithClient(context.Background(), "fs-probe", testProbeImage, dir, client)
	if err == nil || !strings.Contains(err.Error(), "create response lost") {
		t.Fatalf("expected create error, got %v", err)
	}
	if !slices.Equal(client.deleted, []string{"fs-probea"}) {
		t.Fatalf("ambiguous create was not cleaned: %v", client.deleted)
	}
	result := readProbeFile(t, dir, "result.json")
	if !slices.Equal(result.Attempted, []string{"fs-probea"}) || len(result.Created) != 0 {
		t.Fatalf("incorrect create evidence: %+v", result)
	}
}

func TestGatewayProbeDetectsCorruptOutputAndCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	client := &fakeProbeClient{corruptDownload: true, failDelete: true}
	err := gatewayProbeWithClient(context.Background(), "fs-probe", testProbeImage, dir, client)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || !strings.Contains(err.Error(), "remote delete failed") {
		t.Fatalf("expected both verification and cleanup errors, got %v", err)
	}
	result := readProbeFile(t, dir, "result.json")
	if len(result.CleanupErrors) != 2 || len(result.Deleted) != 0 {
		t.Fatalf("cleanup evidence incomplete: %+v", result)
	}
}

func TestGatewayProbeUploadFailureAndCancellation(t *testing.T) {
	dir := t.TempDir()
	client := &fakeProbeClient{failUpload: true}
	if err := gatewayProbeWithClient(context.Background(), "fs-probe", testProbeImage, dir, client); err == nil || !strings.Contains(err.Error(), "upload") {
		t.Fatalf("expected upload error, got %v", err)
	}
	if len(client.deleted) != 2 {
		t.Fatalf("upload failure left sandboxes: %v", client.deleted)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client = &fakeProbeClient{}
	if err := gatewayProbeWithClient(canceled, "fs-cancel", testProbeImage, t.TempDir(), client); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if len(client.created) != 0 {
		t.Fatalf("created after cancellation: %v", client.created)
	}
}

func TestGatewayProbeRejectsInvalidInputBeforeGatewayAccess(t *testing.T) {
	for _, tc := range []struct{ name, image, output string }{
		{"other", "image@sha256:" + strings.Repeat("a", 64), t.TempDir()},
		{"fs-valid", "image:latest", t.TempDir()},
		{"fs-valid", "image@sha256:" + strings.Repeat("a", 64), ""},
	} {
		if err := GatewayProbe(context.Background(), tc.name, tc.image, tc.output); err == nil {
			t.Fatalf("accepted invalid input: %+v", tc)
		}
	}
}

func TestGatewayProbePreflightStopsBeforeRemoteEffects(t *testing.T) {
	for _, tc := range []struct {
		client *fakeProbeClient
		want   string
	}{
		{&fakeProbeClient{failAvailable: true}, "CLI missing"},
		{&fakeProbeClient{failGateway: true}, "gateway unavailable"},
	} {
		err := gatewayProbeWithClient(context.Background(), "fs-probe", testProbeImage, t.TempDir(), tc.client)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %s, got %v", tc.want, err)
		}
		if len(tc.client.created) != 0 {
			t.Fatal("created sandbox before preflight")
		}
	}
}

func TestProbeEvidenceWriteFailsWithoutReplacingPriorResult(t *testing.T) {
	dir := t.TempDir()
	if err := writeEvidenceJSON(dir, "result.json", make(chan int)); err == nil {
		t.Fatal("accepted non-JSON evidence")
	}
	if err := writeEvidenceJSON(filepath.Join(dir, "missing"), "result.json", gatewayProbeResult{}); err == nil {
		t.Fatal("accepted missing evidence directory")
	}
	if err := os.Mkdir(filepath.Join(dir, "result.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeEvidenceJSON(dir, "result.json", gatewayProbeResult{}); err == nil {
		t.Fatal("overwrote directory with evidence file")
	}
}

func TestGatewayProbeChecksClientBeforeWritingEvidence(t *testing.T) {
	binDir := t.TempDir()
	stub := "#!/bin/sh\nif [ \"$1 $2\" = 'gateway list' ]; then echo testing; exit 0; fi\nexit 2\n"
	if err := os.WriteFile(filepath.Join(binDir, "openshell"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENSHELL_GATEWAY_ENDPOINT", "")
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := GatewayProbe(context.Background(), "fs-probe", testProbeImage, blocked); err == nil {
		t.Fatal("expected persistent evidence directory error")
	}
}

func readProbeFile(t *testing.T, dir, name string) gatewayProbeResult {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var result gatewayProbeResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
