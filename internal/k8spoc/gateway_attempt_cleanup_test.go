package k8spoc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayAttemptCleanupConfirmsAbsenceAndWritesEvidence(t *testing.T) {
	jobName := "fs-poc-23-2"
	name, err := attemptSandboxName(jobName)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), jobName)
	listing := []string{name}
	deletes := 0
	err = gatewayAttemptCleanupWithClient(context.Background(), jobName, dir, func(context.Context) ([]string, error) {
		return listing, nil
	}, func(candidate string) error {
		if candidate != name {
			t.Fatalf("deleted foreign sandbox %q", candidate)
		}
		deletes++
		listing = nil
		return nil
	})
	if err != nil || deletes != 1 {
		t.Fatalf("cleanup err=%v deletes=%d", err, deletes)
	}
	result, err := readCleanupResult(filepath.Join(dir, "cleanup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Job != jobName || len(result.Deleted) != 1 || result.Deleted[0] != name {
		t.Fatalf("incorrect stop evidence: %+v", result)
	}
}

func TestGatewayAttemptCleanupRetainsReservationWithoutProof(t *testing.T) {
	jobName := "fs-poc-23-2"
	name, err := attemptSandboxName(jobName)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), jobName)
	if err := gatewayAttemptCleanupWithClient(context.Background(), jobName, dir, func(context.Context) ([]string, error) {
		return nil, errors.New("gateway unavailable")
	}, func(string) error { t.Fatal("delete must not follow an uncertain list"); return nil }); err == nil {
		t.Fatal("accepted failed remote lookup")
	}
	if _, err := os.Stat(filepath.Join(dir, "cleanup.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote unverified stop evidence: %v", err)
	}
	if err := gatewayAttemptCleanupWithClient(context.Background(), "fs-demo-deadbeef", dir, nil, nil); err == nil {
		t.Fatal("accepted foreign Job identity")
	}
	if err := gatewayAttemptCleanupWithClient(context.Background(), jobName, t.TempDir(), nil, nil); err == nil {
		t.Fatal("accepted mismatched evidence path")
	}
	if err := gatewayAttemptCleanupWithClient(context.Background(), jobName, dir, func(context.Context) ([]string, error) {
		return []string{name}, nil
	}, func(string) error { return errors.New("remote delete failed") }); err == nil {
		t.Fatal("accepted failed remote delete")
	}
	if _, err := os.Stat(filepath.Join(dir, "cleanup.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote stop evidence after failed delete: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gatewayAttemptCleanupWithClient(canceled, jobName, dir, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored canceled cleanup context: %v", err)
	}
}

func TestGatewayAttemptCleanupUsesOpenShellAndPersistsRemoteStop(t *testing.T) {
	jobName := "fs-poc-23-2"
	name, err := attemptSandboxName(jobName)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "sandbox")
	if err := os.WriteFile(marker, []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\ncase \"$1 $2\" in\n  'gateway list') echo testing ;;\n  'gateway login') exit 0 ;;\n  'sandbox list') if test -f \"$SANDBOX_MARKER\"; then cat \"$SANDBOX_MARKER\"; fi ;;\n  'sandbox delete') rm -f \"$SANDBOX_MARKER\" ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "openshell"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SANDBOX_MARKER", marker)
	t.Setenv("OPENSHELL_GATEWAY_ENDPOINT", "")
	t.Setenv("OPENSHELL_OIDC_CLIENT_SECRET", "test-secret")
	dir := filepath.Join(t.TempDir(), jobName)
	if err := GatewayAttemptCleanup(context.Background(), jobName, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote sandbox marker still exists: %v", err)
	}
	result, err := readCleanupResult(filepath.Join(dir, "cleanup.json"))
	if err != nil || len(result.Deleted) != 1 || result.Deleted[0] != name {
		t.Fatalf("missing remote stop evidence: result=%+v err=%v", result, err)
	}
	if err := os.WriteFile(marker, []byte("invalid sandbox name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listGatewaySandboxes(context.Background()); err == nil {
		t.Fatal("accepted malformed sandbox listing as absence")
	}
	if err := os.WriteFile(marker, []byte(strings.Repeat(name+"\n", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listGatewaySandboxes(context.Background()); err == nil {
		t.Fatal("accepted potentially truncated sandbox listing as absence")
	}
}

func TestGatewayAttemptCleanupRejectsMissingClient(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := GatewayAttemptCleanup(context.Background(), "fs-poc-1-1", filepath.Join(t.TempDir(), "fs-poc-1-1")); err == nil {
		t.Fatal("accepted missing OpenShell binary")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "openshell"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENSHELL_OIDC_CLIENT_SECRET", "")
	if err := GatewayAttemptCleanup(context.Background(), "fs-poc-1-1", filepath.Join(t.TempDir(), "fs-poc-1-1")); err == nil {
		t.Fatal("accepted missing gateway service-account secret")
	}
	t.Setenv("OPENSHELL_OIDC_CLIENT_SECRET", "test-secret")
	t.Setenv("OPENSHELL_GATEWAY_ENDPOINT", "")
	if err := GatewayAttemptCleanup(context.Background(), "fs-poc-1-1", filepath.Join(t.TempDir(), "fs-poc-1-1")); err == nil {
		t.Fatal("accepted an unconfigured gateway")
	}
}
