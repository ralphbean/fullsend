package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fullsend-ai/fullsend/internal/mintclient"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

func TestUploadGitHubCredentialStagesAndAtomicallyReplacesToken(t *testing.T) {
	oldUpload, oldExec := uploadSandboxFile, execSandboxCommand
	t.Cleanup(func() { uploadSandboxFile, execSandboxCommand = oldUpload, oldExec })
	var stagedToken string
	uploadSandboxFile = func(_ string, localPath, remotePath string) error {
		if remotePath != githubCredentialNextPath {
			t.Fatalf("uploaded to %q, want staging path %q", remotePath, githubCredentialNextPath)
		}
		info, err := os.Stat(localPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("temporary token permissions = %o, want 600", info.Mode().Perm())
		}
		data, err := os.ReadFile(localPath)
		if err != nil {
			t.Fatal(err)
		}
		stagedToken = string(data)
		return nil
	}
	var command string
	execSandboxCommand = func(_ context.Context, sandboxName, gotCommand string, _ time.Duration) (string, string, int, error) {
		if sandboxName != "sandbox" {
			t.Fatalf("sandbox = %q, want sandbox", sandboxName)
		}
		command = gotCommand
		return "", "", 0, nil
	}

	if err := uploadGitHubCredential(context.Background(), "sandbox", "ghs_test_refresh_token"); err != nil {
		t.Fatal(err)
	}
	if stagedToken != "ghs_test_refresh_token\n" {
		t.Fatalf("staged credential = %q", stagedToken)
	}
	if !strings.Contains(command, "mv -f "+githubCredentialNextPath+" "+githubCredentialPath) {
		t.Fatalf("credential replacement is not atomic: %q", command)
	}
	if strings.Contains(command, "ghs_test_refresh_token") {
		t.Fatal("credential appeared in sandbox command")
	}
}

func TestGitHubCredentialHelperIsScopedToGitHubHTTPS(t *testing.T) {
	if !strings.Contains(gitHubCredentialHelperScript, `[ "$protocol" = https ] && [ "$host" = github.com ]`) {
		t.Fatal("Git credential helper does not restrict credentials to GitHub HTTPS")
	}
	if !strings.Contains(gitHubCredentialHelperScript, "installation-token") {
		t.Fatal("Git credential helper does not read the rotating token file")
	}
}

func TestWriteInitialGitHubCredentialInstallsHelperAndToken(t *testing.T) {
	oldUpload, oldExec := uploadSandboxFile, execSandboxCommand
	t.Cleanup(func() { uploadSandboxFile, execSandboxCommand = oldUpload, oldExec })
	var uploaded []string
	var commands []string
	uploadSandboxFile = func(_ string, localPath, remotePath string) error {
		uploaded = append(uploaded, remotePath)
		if remotePath == githubCredentialHelper {
			data, err := os.ReadFile(localPath)
			if err != nil {
				return err
			}
			if !strings.Contains(string(data), "github.com") {
				t.Fatal("uploaded credential helper is missing host restriction")
			}
		}
		return nil
	}
	execSandboxCommand = func(_ context.Context, _, command string, _ time.Duration) (string, string, int, error) {
		commands = append(commands, command)
		return "", "", 0, nil
	}
	if err := writeInitialGitHubCredential(context.Background(), "sandbox", "ghs_initial_token"); err != nil {
		t.Fatal(err)
	}
	if len(uploaded) != 2 || uploaded[0] != githubCredentialHelper || uploaded[1] != githubCredentialNextPath {
		t.Fatalf("uploaded paths = %v", uploaded)
	}
	if len(commands) != 4 || !strings.Contains(commands[1], "credential.helper") || !strings.Contains(commands[3], "mv -f") {
		t.Fatalf("sandbox commands = %v", commands)
	}
}

func TestRefreshGitHubInstallationTokenClearsCredentialOnDenial(t *testing.T) {
	oldMint, oldExec := statusMintToken, execSandboxCommand
	t.Cleanup(func() { statusMintToken, execSandboxCommand = oldMint, oldExec })
	statusMintToken = func(_ context.Context, request mintclient.MintRequest) (*mintclient.MintResult, error) {
		if request.RepositoryID != 123456 || request.Role != "coder" || request.Level != "write" || request.KubernetesTokenFile != "/projected/token" {
			t.Fatalf("mint request = %+v", request)
		}
		return nil, errors.New("denied")
	}
	var clearCommand string
	execSandboxCommand = func(_ context.Context, _, command string, _ time.Duration) (string, string, int, error) {
		clearCommand = command
		return "", "", 0, nil
	}
	err := refreshGitHubInstallationToken(context.Background(), "coder", "http://mint.svc", "write", 123456, "/projected/token", "sandbox")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("refresh error = %v, want mint denial", err)
	}
	if !strings.Contains(clearCommand, "rm -f") || !strings.Contains(clearCommand, githubCredentialPath) {
		t.Fatalf("failed refresh did not clear sandbox credential: %q", clearCommand)
	}
}

func TestRefreshGitHubInstallationTokenFailsClosedOnBadTokenAndUpload(t *testing.T) {
	oldMint, oldUpload, oldClear := statusMintToken, uploadGitHubCredential, clearGitHubCredential
	t.Cleanup(func() { statusMintToken, uploadGitHubCredential, clearGitHubCredential = oldMint, oldUpload, oldClear })
	clearCalls := 0
	clearGitHubCredential = func(context.Context, string) error {
		clearCalls++
		return nil
	}
	t.Run("invalid token format", func(t *testing.T) {
		statusMintToken = func(context.Context, mintclient.MintRequest) (*mintclient.MintResult, error) {
			return &mintclient.MintResult{Token: "not-a-github-token"}, nil
		}
		if err := refreshGitHubInstallationToken(context.Background(), "coder", "https://mint.example", "write", 123456, "/token", "sandbox"); err == nil {
			t.Fatal("malformed token was accepted")
		}
		if clearCalls != 1 {
			t.Fatalf("clear calls = %d, want 1", clearCalls)
		}
	})
	t.Run("credential upload failure", func(t *testing.T) {
		statusMintToken = func(context.Context, mintclient.MintRequest) (*mintclient.MintResult, error) {
			return &mintclient.MintResult{Token: "ghs_valid_rotated_token"}, nil
		}
		uploadGitHubCredential = func(context.Context, string, string) error { return errors.New("upload failed") }
		if err := refreshGitHubInstallationToken(context.Background(), "coder", "https://mint.example", "write", 123456, "/token", "sandbox"); err == nil || !strings.Contains(err.Error(), "upload failed") {
			t.Fatalf("error = %v, want upload failure", err)
		}
		if clearCalls != 2 {
			t.Fatalf("clear calls = %d, want 2", clearCalls)
		}
	})
	t.Run("credential clear failure", func(t *testing.T) {
		statusMintToken = func(context.Context, mintclient.MintRequest) (*mintclient.MintResult, error) {
			return nil, errors.New("mint denied")
		}
		clearGitHubCredential = func(context.Context, string) error { return errors.New("clear failed") }
		err := refreshGitHubInstallationToken(context.Background(), "coder", "https://mint.example", "write", 123456, "/token", "sandbox")
		if err == nil || !strings.Contains(err.Error(), "clearing sandbox credential failed") {
			t.Fatalf("error = %v, want fail-closed clear error", err)
		}
	})
}

func TestWriteInitialGitHubCredentialPropagatesSetupErrors(t *testing.T) {
	oldInstall, oldUpload := installGitHubCredentialHelper, uploadGitHubCredential
	t.Cleanup(func() { installGitHubCredentialHelper, uploadGitHubCredential = oldInstall, oldUpload })
	installGitHubCredentialHelper = func(context.Context, string) error { return errors.New("helper failed") }
	uploadGitHubCredential = func(context.Context, string, string) error {
		t.Fatal("token upload should not run after helper failure")
		return nil
	}
	if err := writeInitialGitHubCredential(context.Background(), "sandbox", "ghs_token"); err == nil || !strings.Contains(err.Error(), "helper failed") {
		t.Fatalf("error = %v, want helper setup error", err)
	}
	installGitHubCredentialHelper = func(context.Context, string) error { return nil }
	uploadGitHubCredential = func(context.Context, string, string) error { return errors.New("token upload failed") }
	if err := writeInitialGitHubCredential(context.Background(), "sandbox", "ghs_token"); err == nil || !strings.Contains(err.Error(), "token upload failed") {
		t.Fatalf("error = %v, want token upload error", err)
	}
}

func TestRunGitHubTokenRefreshRotatesRepeatedly(t *testing.T) {
	oldMint, oldUpload, oldInterval := statusMintToken, uploadGitHubCredential, githubTokenRefreshInterval
	t.Cleanup(func() {
		statusMintToken, uploadGitHubCredential, githubTokenRefreshInterval = oldMint, oldUpload, oldInterval
	})
	githubTokenRefreshInterval = time.Millisecond
	var count int
	statusMintToken = func(context.Context, mintclient.MintRequest) (*mintclient.MintResult, error) {
		count++
		return &mintclient.MintResult{Token: "ghs_rotated_token", ExpiresAt: "2099-01-01T00:00:00Z"}, nil
	}
	rotations := make(chan string, 3)
	uploadGitHubCredential = func(_ context.Context, _, token string) error {
		rotations <- token
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runGitHubTokenRefresh(ctx, "sandbox", "coder", "http://mint.svc", "write", 123456, "/projected/token")
	}()
	for i := 0; i < 2; i++ {
		select {
		case token := <-rotations:
			if token != "ghs_rotated_token" {
				t.Fatalf("rotated token = %q", token)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for token rotation")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("refresh loop error = %v", err)
	}
	if count < 2 {
		t.Fatalf("mint called %d times, want repeated refresh", count)
	}
}

func TestMintAgentTokenAtLevelUsesProjectedRepositoryIdentity(t *testing.T) {
	oldMint := statusMintToken
	t.Cleanup(func() { statusMintToken = oldMint })
	t.Setenv("KUBERNETES_MINT_TOKEN_FILE", "/var/run/secrets/fullsend/mint/token")
	t.Setenv("FULLSEND_REPOSITORY_ID", "123456")
	t.Setenv("REPO_FULL_NAME", "")
	t.Setenv("GH_TOKEN", "")
	statusMintToken = func(_ context.Context, request mintclient.MintRequest) (*mintclient.MintResult, error) {
		if request.RepositoryID != 123456 || request.KubernetesTokenFile != "/var/run/secrets/fullsend/mint/token" || len(request.Repos) != 0 {
			t.Fatalf("Kubernetes mint request = %+v", request)
		}
		return &mintclient.MintResult{Token: "ghs_kubernetes_identity_token", ExpiresAt: "2099-01-01T00:00:00Z"}, nil
	}
	minted, cleanup, err := mintAgentTokenAtLevel(context.Background(), "coder", "http://fullsend-mint.fullsend-mint.svc:8080", "github", "write", ui.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !minted || os.Getenv("GH_TOKEN") != "ghs_kubernetes_identity_token" {
		t.Fatal("projected identity did not mint and set the scoped runner token")
	}
}
