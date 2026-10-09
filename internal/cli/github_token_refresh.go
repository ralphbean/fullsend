package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fullsend-ai/fullsend/internal/mintclient"
	"github.com/fullsend-ai/fullsend/internal/sandbox"
)

const (
	githubCredentialDir      = sandbox.SandboxWorkspace + "/.fullsend-credentials"
	githubCredentialPath     = githubCredentialDir + "/installation-token"
	githubCredentialNextPath = githubCredentialPath + ".next"
	githubCredentialHelper   = githubCredentialDir + "/git-credential-helper"
)

var githubTokenRefreshInterval = 4 * time.Minute

var uploadSandboxFile = sandbox.UploadFile
var execSandboxCommand = sandbox.ExecContext

const gitHubCredentialHelperScript = `#!/bin/sh
set -eu
[ "${1:-}" = get ] || exit 0
protocol=''
host=''
while IFS='=' read -r key value; do
  case "$key" in
    protocol) protocol="$value" ;;
    host) host="$value" ;;
  esac
done
[ "$protocol" = https ] && [ "$host" = github.com ] || exit 0
printf 'username=x-access-token\npassword='
cat /sandbox/workspace/.fullsend-credentials/installation-token
printf '\n\n'
`

var uploadGitHubCredential = func(ctx context.Context, sandboxName, token string) error {
	if token == "" {
		return fmt.Errorf("GitHub installation token is empty")
	}
	tmp, err := os.CreateTemp("", "fullsend-github-token-*")
	if err != nil {
		return fmt.Errorf("creating temporary credential file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("restricting temporary credential permissions: %w", err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temporary credential: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary credential: %w", err)
	}
	prepare := "mkdir -p " + githubCredentialDir + " && chmod 700 " + githubCredentialDir
	if _, _, _, err := execSandboxCommand(ctx, sandboxName, prepare, 30*time.Second); err != nil {
		return fmt.Errorf("preparing sandbox credential directory: %w", err)
	}
	if err := uploadSandboxFile(sandboxName, tmpName, githubCredentialNextPath); err != nil {
		return fmt.Errorf("uploading refreshed GitHub credential: %w", err)
	}
	command := "chmod 600 " + githubCredentialNextPath + " && mv -f " + githubCredentialNextPath + " " + githubCredentialPath
	if _, _, _, err := execSandboxCommand(ctx, sandboxName, command, 30*time.Second); err != nil {
		return fmt.Errorf("atomically replacing sandbox GitHub credential: %w", err)
	}
	return nil
}

var clearGitHubCredential = func(ctx context.Context, sandboxName string) error {
	_, _, _, err := execSandboxCommand(ctx, sandboxName, "rm -f "+githubCredentialNextPath+" "+githubCredentialPath, 30*time.Second)
	return err
}

var installGitHubCredentialHelper = func(ctx context.Context, sandboxName string) error {
	tmp, err := os.CreateTemp("", "fullsend-git-credential-helper-*")
	if err != nil {
		return fmt.Errorf("creating temporary Git credential helper: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(gitHubCredentialHelperScript); err != nil {
		tmp.Close()
		return fmt.Errorf("writing Git credential helper: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing Git credential helper: %w", err)
	}
	prepare := "mkdir -p " + githubCredentialDir + " && chmod 700 " + githubCredentialDir
	if _, _, _, err := execSandboxCommand(ctx, sandboxName, prepare, 30*time.Second); err != nil {
		return fmt.Errorf("preparing sandbox credential directory: %w", err)
	}
	if err := uploadSandboxFile(sandboxName, tmpName, githubCredentialHelper); err != nil {
		return fmt.Errorf("uploading Git credential helper: %w", err)
	}
	command := "chmod 700 " + githubCredentialHelper + " && git config --global credential.helper " + githubCredentialHelper
	if _, _, _, err := execSandboxCommand(ctx, sandboxName, command, 30*time.Second); err != nil {
		return fmt.Errorf("installing Git credential helper: %w", err)
	}
	return nil
}

func refreshGitHubInstallationToken(ctx context.Context, role, mintURL, level string, repositoryID int64, tokenFile, sandboxName string) error {
	if repositoryID <= 0 || tokenFile == "" {
		return fmt.Errorf("Kubernetes mint repository ID and projected token file are required")
	}
	result, err := statusMintToken(ctx, mintclient.MintRequest{
		MintURL: mintURL, Role: role, Level: level, RepositoryID: repositoryID,
		KubernetesTokenFile: tokenFile,
	})
	if err == nil && result != nil && mintTokenPattern.MatchString(result.Token) {
		err = uploadGitHubCredential(ctx, sandboxName, result.Token)
	} else if err == nil {
		err = fmt.Errorf("mint returned a GitHub token with an unexpected format")
	}
	if err != nil {
		clearErr := clearGitHubCredential(context.WithoutCancel(ctx), sandboxName)
		if clearErr != nil {
			return fmt.Errorf("refreshing GitHub token failed (%v) and clearing sandbox credential failed: %w", err, clearErr)
		}
		return err
	}
	return nil
}

func runGitHubTokenRefresh(ctx context.Context, sandboxName, role, mintURL, level string, repositoryID int64, tokenFile string) error {
	ticker := time.NewTicker(githubTokenRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := refreshGitHubInstallationToken(ctx, role, mintURL, level, repositoryID, tokenFile, sandboxName); err != nil {
				return fmt.Errorf("GitHub installation token refresh failed: %w", err)
			}
		}
	}
}

func writeInitialGitHubCredential(ctx context.Context, sandboxName, token string) error {
	if err := installGitHubCredentialHelper(ctx, sandboxName); err != nil {
		return err
	}
	return uploadGitHubCredential(ctx, sandboxName, token)
}
