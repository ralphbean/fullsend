package k8spoc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGatewayCleanupInterruptedProbe(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fs-gateway-deadbeef")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProbeResult(dir, "intent.json", gatewayProbeResult{Names: []string{"fs-deadbeefa", "fs-deadbeefb"}}); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	if err := gatewayCleanupWithDelete(context.Background(), root, func(name string) error {
		deleted = append(deleted, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"fs-deadbeefa", "fs-deadbeefb"}) {
		t.Fatalf("deleted %v", deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "cleanup.json")); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayCleanupInterruptedDemo(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fs-demo-deadbeef")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeEvidenceJSON(dir, "intent.json", gatewayDemoResult{Names: []string{"fs-deadbeef"}}); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	if err := gatewayCleanupWithDelete(context.Background(), root, func(name string) error {
		deleted = append(deleted, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"fs-deadbeef"}) {
		t.Fatalf("deleted %v", deleted)
	}
}

func TestGatewayCleanupSkipsProvenDeletion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fs-gateway-deadbeef")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := gatewayProbeResult{Names: []string{"fs-deadbeefa", "fs-deadbeefb"}}
	if err := writeProbeResult(dir, "intent.json", intent); err != nil {
		t.Fatal(err)
	}
	if err := writeProbeResult(dir, "result.json", gatewayProbeResult{Deleted: []string{"fs-deadbeefa"}}); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	if err := gatewayCleanupWithDelete(context.Background(), root, func(name string) error {
		deleted = append(deleted, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"fs-deadbeefb"}) {
		t.Fatalf("deleted %v", deleted)
	}
}

func TestGatewayCleanupRejectsMismatchedIntent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fs-gateway-deadbeef")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProbeResult(dir, "intent.json", gatewayProbeResult{Names: []string{"fs-other"}}); err != nil {
		t.Fatal(err)
	}
	err := gatewayCleanupWithDelete(context.Background(), root, func(string) error {
		t.Fatal("should not delete on mismatched intent")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
}

func TestGatewayCleanupRetriesOnlyUnconfirmedDeletion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fs-gateway-deadbeef")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProbeResult(dir, "intent.json", gatewayProbeResult{Names: []string{"fs-deadbeefa", "fs-deadbeefb"}}); err != nil {
		t.Fatal(err)
	}
	var first []string
	err := gatewayCleanupWithDelete(context.Background(), root, func(name string) error {
		first = append(first, name)
		if name == "fs-deadbeefb" {
			return errors.New("temporary gateway failure")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "temporary gateway failure") || len(first) != 2 {
		t.Fatalf("expected partial cleanup failure, got %v, calls %v", err, first)
	}
	var retried []string
	if err := gatewayCleanupWithDelete(context.Background(), root, func(name string) error {
		retried = append(retried, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(retried, []string{"fs-deadbeefb"}) {
		t.Fatalf("retried already deleted sandbox: %v", retried)
	}
}

func TestGatewayCleanupRejectsCorruptResultAndCanceledContext(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fs-gateway-deadbeef")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProbeResult(dir, "intent.json", gatewayProbeResult{Names: []string{"fs-deadbeefa", "fs-deadbeefb"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := gatewayCleanupWithDelete(context.Background(), root, func(string) error {
		t.Fatal("should not delete with corrupt result")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "result") {
		t.Fatalf("expected corrupt result error, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gatewayCleanupWithDelete(ctx, root, func(string) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestGatewayCleanupInvokesOpenShellForOwnedOrphan(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "fs-gateway-deadbeef")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProbeResult(dir, "intent.json", gatewayProbeResult{Names: []string{"fs-deadbeefa", "fs-deadbeefb"}}); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "deleted")
	stub := "#!/bin/sh\ncase \"$1 $2\" in\n  'gateway list') echo testing ;;\n  'gateway login') exit 0 ;;\n  'sandbox delete') echo \"$3\" >> \"$PROBE_DELETE_LOG\" ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "openshell"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PROBE_DELETE_LOG", logPath)
	t.Setenv("OPENSHELL_GATEWAY_ENDPOINT", "")
	t.Setenv("OPENSHELL_OIDC_CLIENT_SECRET", "test-secret")
	if err := GatewayCleanup(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	deleted, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(deleted) != "fs-deadbeefa\nfs-deadbeefb\n" {
		t.Fatalf("unexpected OpenShell deletions: %s", deleted)
	}
}
