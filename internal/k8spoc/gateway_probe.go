package k8spoc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fullsend-ai/fullsend/internal/sandbox"
)

// GatewayProbe exercises the same OpenShell client used by fullsend run. The
// output directory must be a persistent volume: the intent file is written
// before a remote sandbox is created and can be used to find orphans later.
func GatewayProbe(ctx context.Context, name, image, outputDir string) error {
	return gatewayProbeWithClient(ctx, name, image, outputDir, realProbeClient{})
}

type probeClient interface {
	available() error
	checkGateway() error
	create(name, image string) error
	upload(name, localPath, remotePath string) error
	exec(ctx context.Context, name, command string) (string, int, error)
	download(name, remotePath, localPath string) error
	delete(name string) error
}

type realProbeClient struct{}

func (realProbeClient) available() error { return sandbox.EnsureAvailable() }
func (realProbeClient) checkGateway() error {
	if err := refreshGatewayAuth(); err != nil {
		return err
	}
	return sandbox.CheckGateway()
}
func (realProbeClient) create(name, image string) error {
	if err := refreshGatewayAuth(); err != nil {
		return err
	}
	return sandbox.CreateWithRetry(name, nil, image, "/etc/fullsend/probe-policy.yaml", 1, 5*time.Minute)
}
func (realProbeClient) upload(name, localPath, remotePath string) error {
	if err := refreshGatewayAuth(); err != nil {
		return err
	}
	return sandbox.UploadFile(name, localPath, remotePath)
}
func (realProbeClient) exec(ctx context.Context, name, command string) (string, int, error) {
	if err := refreshGatewayAuth(); err != nil {
		return "", 0, err
	}
	_, stderr, exitCode, err := sandbox.ExecContext(ctx, name, command, 30*time.Second)
	return stderr, exitCode, err
}
func (realProbeClient) download(name, remotePath, localPath string) error {
	if err := refreshGatewayAuth(); err != nil {
		return err
	}
	return sandbox.DownloadFile(name, remotePath, localPath)
}
func (realProbeClient) delete(name string) error { return deleteGatewaySandbox(name) }

func gatewayProbeWithClient(ctx context.Context, name, image, outputDir string, client probeClient) (resultErr error) {
	if !validProbeName(name) {
		return fmt.Errorf("probe name must be fs- followed by lowercase letters, digits or hyphens, with at most 17 characters")
	}
	if !containsDigest(image) {
		return fmt.Errorf("sandbox image must be pinned by sha256 digest")
	}
	if outputDir == "" {
		return fmt.Errorf("persistent output directory is required")
	}
	if err := client.available(); err != nil {
		return err
	}
	if err := client.checkGateway(); err != nil {
		return err
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return err
	}
	result := gatewayProbeResult{Names: []string{name + "a", name + "b"}, Image: image, StartedAt: time.Now().UTC()}
	if err := writeProbeResult(outputDir, "intent.json", result); err != nil {
		return err
	}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if resultErr != nil {
			result.Error = resultErr.Error()
		}
		if err := writeProbeResult(outputDir, "result.json", result); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		if summary, err := json.Marshal(result); err == nil {
			fmt.Println(string(summary))
		}
	}()
	defer func() {
		for _, sandboxName := range result.Attempted {
			if err := client.delete(sandboxName); err != nil {
				result.CleanupErrors = append(result.CleanupErrors, fmt.Sprintf("%s: %v", sandboxName, err))
				resultErr = errors.Join(resultErr, fmt.Errorf("delete %s: %w", sandboxName, err))
			} else {
				result.Deleted = append(result.Deleted, sandboxName)
			}
		}
	}()

	// The two sandboxes overlap so the probe demonstrates separate workspaces.
	// Each name is saved before its create request; deletion is attempted even
	// if create returned an ambiguous error.
	for _, sandboxName := range result.Names {
		if err := ctx.Err(); err != nil {
			return err
		}
		result.Attempted = append(result.Attempted, sandboxName)
		if err := client.create(sandboxName, image); err != nil {
			return fmt.Errorf("create %s: %w", sandboxName, err)
		}
		result.Created = append(result.Created, sandboxName)
	}
	for i, sandboxName := range result.Names {
		input := []byte(fmt.Sprintf("fullsend gateway probe %s %d\n", sandboxName, i))
		localIn := filepath.Join(outputDir, sandboxName+".input")
		localOut := filepath.Join(outputDir, sandboxName+".output")
		if err := os.WriteFile(localIn, input, 0o600); err != nil {
			return err
		}
		if err := client.upload(sandboxName, localIn, "/sandbox/workspace/probe.input"); err != nil {
			return fmt.Errorf("upload %s: %w", sandboxName, err)
		}
		stderr, exitCode, err := client.exec(ctx, sandboxName, "cp /sandbox/workspace/probe.input /sandbox/workspace/probe.output")
		if err != nil {
			return fmt.Errorf("exec %s: exit %d: %s: %w", sandboxName, exitCode, stderr, err)
		}
		if exitCode != 0 {
			return fmt.Errorf("exec %s: exit %d: %s", sandboxName, exitCode, stderr)
		}
		if err := client.download(sandboxName, "/sandbox/workspace/probe.output", localOut); err != nil {
			return fmt.Errorf("download %s: %w", sandboxName, err)
		}
		got, err := os.ReadFile(localOut)
		if err != nil {
			return err
		}
		wantHash, gotHash := sha256.Sum256(input), sha256.Sum256(got)
		if gotHash != wantHash {
			return fmt.Errorf("output checksum mismatch for %s", sandboxName)
		}
		result.Transfers = append(result.Transfers, probeTransfer{Sandbox: sandboxName, SHA256: hex.EncodeToString(gotHash[:]), ExitCode: exitCode})
	}
	return nil
}

type gatewayProbeResult struct {
	Names         []string        `json:"names"`
	Image         string          `json:"image"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at,omitempty"`
	Attempted     []string        `json:"attempted,omitempty"`
	Created       []string        `json:"created,omitempty"`
	Transfers     []probeTransfer `json:"transfers,omitempty"`
	Deleted       []string        `json:"deleted,omitempty"`
	CleanupErrors []string        `json:"cleanup_errors,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type probeTransfer struct {
	Sandbox  string `json:"sandbox"`
	SHA256   string `json:"sha256"`
	ExitCode int    `json:"exit_code"`
}

func writeProbeResult(dir, filename string, value gatewayProbeResult) error {
	return writeEvidenceJSON(dir, filename, value)
}

func writeEvidenceJSON(dir, filename string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, filename)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
