package k8spoc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// GatewayDemo runs the pinned, local demo harness through the normal
// `fullsend run` command. Its intent and output stay on the evidence PVC.
func GatewayDemo(ctx context.Context, jobName, sandboxName, demoDir, evidenceDir string) error {
	run := func(ctx context.Context, outputDir string) error {
		return runGatewayDemoCommand(ctx, "/usr/local/bin/fullsend", demoDir, outputDir, sandboxName)
	}
	return gatewayDemoWithRunner(ctx, jobName, sandboxName, evidenceDir, run, deleteGatewaySandbox)
}

func runGatewayDemoCommand(ctx context.Context, binary, demoDir, outputDir, sandboxName string) error {
	cmd := exec.CommandContext(ctx, binary, "run", "gateway-demo",
		"--fullsend-dir", filepath.Join(demoDir, ".fullsend"),
		"--target-repo", filepath.Join(demoDir, "repo"),
		"--output-dir", outputDir,
		"--sandbox-name", sandboxName,
		"--offline", "--no-post-script", "--keep-sandbox")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

type gatewayDemoResult struct {
	Names      []string  `json:"names"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	OutputSHA  string    `json:"output_sha256,omitempty"`
	Deleted    []string  `json:"deleted,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func gatewayDemoWithRunner(ctx context.Context, jobName, sandboxName, evidenceDir string, run func(context.Context, string) error, deleteSandbox func(string) error) (resultErr error) {
	derived, ok := ownedGatewayNames(jobName)
	if !ok {
		if name, err := attemptSandboxName(jobName); err == nil {
			derived = []string{name}
			ok = true
		}
	}
	if !ok || len(derived) != 1 || derived[0] != sandboxName {
		return fmt.Errorf("demo Job and sandbox names must have matching owned identities")
	}
	if evidenceDir == "" {
		return fmt.Errorf("persistent evidence directory is required")
	}
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return err
	}
	result := gatewayDemoResult{Names: []string{sandboxName}, StartedAt: time.Now().UTC()}
	if err := writeEvidenceJSON(evidenceDir, "intent.json", result); err != nil {
		return err
	}
	defer func() {
		if err := deleteSandbox(sandboxName); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("delete %s: %w", sandboxName, err))
		} else {
			result.Deleted = append(result.Deleted, sandboxName)
		}
		result.FinishedAt = time.Now().UTC()
		if resultErr != nil {
			result.Error = resultErr.Error()
		}
		if err := writeEvidenceJSON(evidenceDir, "result.json", result); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		if summary, err := json.Marshal(result); err == nil {
			fmt.Println(string(summary))
		}
	}()
	outputBase := filepath.Join(evidenceDir, "runs")
	if err := run(ctx, outputBase); err != nil {
		return fmt.Errorf("fullsend run gateway-demo: %w", err)
	}
	output := filepath.Join(outputBase, sandboxName, "iteration-1", "output", "agent-result.json")
	b, err := os.ReadFile(output)
	if err != nil {
		return fmt.Errorf("read demo result: %w", err)
	}
	var document struct {
		Status string `json:"status"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(b, &document); err != nil {
		return fmt.Errorf("invalid demo result JSON: %w", err)
	}
	if document.Status != "ok" || document.Source != "kind-gateway-demo" {
		return fmt.Errorf("demo result did not match the expected status and source")
	}
	hash := sha256.Sum256(b)
	result.OutputSHA = hex.EncodeToString(hash[:])
	return nil
}
