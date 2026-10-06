package k8spoc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fullsend-ai/fullsend/internal/harness"
	agentruntime "github.com/fullsend-ai/fullsend/internal/runtime"
)

func TestGatewayDemoPersistsAndValidatesRunOutput(t *testing.T) {
	dir := t.TempDir()
	called := false
	run := func(_ context.Context, outputDir string) error {
		if _, err := os.Stat(filepath.Join(dir, "intent.json")); err != nil {
			t.Fatalf("intent was not durable before run: %v", err)
		}
		output := filepath.Join(outputDir, "fs-deadbeef", "iteration-1", "output", "agent-result.json")
		if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			return err
		}
		return os.WriteFile(output, []byte(`{"status":"ok","source":"kind-gateway-demo"}`), 0o600)
	}
	err := gatewayDemoWithRunner(context.Background(), "fs-demo-deadbeef", "fs-deadbeef", dir, run, func(name string) error {
		called = true
		if name != "fs-deadbeef" {
			t.Fatalf("unexpected sandbox %s", name)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("demo error %v, cleanup called %v", err, called)
	}
	var result gatewayDemoResult
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != "fs-deadbeef" || len(result.OutputSHA) != 64 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestGatewayDemoFailureKeepsIntentAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	err := gatewayDemoWithRunner(context.Background(), "fs-demo-deadbeef", "fs-deadbeef", dir,
		func(context.Context, string) error { return errors.New("run failed") },
		func(string) error { return errors.New("delete failed") })
	if err == nil || !strings.Contains(err.Error(), "run failed") || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("expected both failures, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "intent.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayDemoRejectsForeignIdentity(t *testing.T) {
	called := false
	err := gatewayDemoWithRunner(context.Background(), "fs-demo-deadbeef", "fs-other", t.TempDir(),
		func(context.Context, string) error { called = true; return nil },
		func(string) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("expected rejection before effects, got %v", err)
	}
	if err := GatewayDemo(context.Background(), "fs-demo-deadbeef", "fs-other", t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("exported demo accepted a foreign sandbox identity")
	}
}

func TestRunGatewayDemoCommandUsesFullsendRun(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fullsend")
	argsFile := filepath.Join(dir, "args")
	t.Setenv("FULLSEND_DEMO_ARGS_FILE", argsFile)
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FULLSEND_DEMO_ARGS_FILE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runGatewayDemoCommand(context.Background(), binary, dir, filepath.Join(dir, "evidence"), "fs-deadbeef"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(b)
	for _, wanted := range []string{"run\ngateway-demo\n", "--fullsend-dir\n" + filepath.Join(dir, ".fullsend"), "--sandbox-name\nfs-deadbeef", "--keep-sandbox"} {
		if !strings.Contains(args, wanted) {
			t.Fatalf("missing %q in command args: %s", wanted, args)
		}
	}
}

func TestGatewayDemoFixtureLoadsWithoutRemoteResources(t *testing.T) {
	dir := filepath.Join("..", "..", "k8s", "fixtures", "harness-demo", ".fullsend")
	h, err := harness.Load(filepath.Join(dir, "harness", "gateway-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ResolveRelativeTo(dir); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := h.ValidateFilesExist(); err != nil {
		t.Fatal(err)
	}
	if _, err := agentruntime.LoadBehaviourScript(filepath.Join(dir, "behaviour", "current-scenario.yaml")); err != nil {
		t.Fatal(err)
	}
}
