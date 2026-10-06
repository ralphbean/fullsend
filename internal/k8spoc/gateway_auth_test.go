package k8spoc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshGatewayAuth(t *testing.T) {
	t.Run("requires service account secret", func(t *testing.T) {
		t.Setenv("OPENSHELL_OIDC_CLIENT_SECRET", "")
		if err := refreshGatewayAuth(); err == nil {
			t.Fatal("missing secret must fail before invoking openshell")
		}
	})
	t.Run("runs client credentials login", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "login-args")
		cli := filepath.Join(dir, "openshell")
		if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$AUTH_MARKER\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("AUTH_MARKER", marker)
		t.Setenv("OPENSHELL_OIDC_CLIENT_SECRET", "test-secret")
		if err := refreshGatewayAuth(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "gateway login" {
			t.Fatalf("unexpected openshell invocation: %q", got)
		}
	})
}

func TestRealProbeClientRefreshesLoginBeforeOperations(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "login-count")
	cli := filepath.Join(dir, "openshell")
	stub := "#!/bin/sh\ncase \"$1 $2\" in\n  'gateway login') echo login >> \"$AUTH_MARKER\" ;;\n  'gateway list') echo poc-runner ;;\n  'sandbox delete') exit 0 ;;\n  *) exit 4 ;;\nesac\n"
	if err := os.WriteFile(cli, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUTH_MARKER", marker)
	t.Setenv("OPENSHELL_OIDC_CLIENT_SECRET", "test-secret")
	t.Setenv("OPENSHELL_GATEWAY_ENDPOINT", "")
	client := realProbeClient{}
	if err := client.checkGateway(); err != nil {
		t.Fatal(err)
	}
	if err := client.create("fs-probea", "sandbox@sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("create stub should fail")
	}
	if err := client.upload("fs-probea", input, "/sandbox/workspace/input"); err == nil {
		t.Fatal("upload stub should fail")
	}
	if _, exitCode, err := client.exec(context.Background(), "fs-probea", "true"); err == nil && exitCode == 0 {
		t.Fatal("exec stub should return a failing exit code")
	}
	if err := client.download("fs-probea", "/sandbox/workspace/output", filepath.Join(dir, "output")); err == nil {
		t.Fatal("download stub should fail")
	}
	if err := client.delete("fs-probea"); err != nil {
		t.Fatal(err)
	}
	logins, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(logins), "login\n"); got != 6 {
		t.Fatalf("expected a fresh login before each operation, got %d", got)
	}
}
