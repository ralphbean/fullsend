package k8spoc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/sandbox"
)

// GatewayAttemptCleanup is run by a separate Job after the original runner
// Pod is gone. A successful Job proves that the derived sandbox is absent.
func GatewayAttemptCleanup(ctx context.Context, jobName, evidenceDir string) error {
	if err := sandbox.EnsureAvailable(); err != nil {
		return err
	}
	if err := refreshGatewayAuth(); err != nil {
		return err
	}
	if err := sandbox.CheckGateway(); err != nil {
		return err
	}
	return gatewayAttemptCleanupWithClient(ctx, jobName, evidenceDir, listGatewaySandboxes, deleteGatewaySandbox)
}

func gatewayAttemptCleanupWithClient(ctx context.Context, jobName, evidenceDir string, list func(context.Context) ([]string, error), deleteSandbox func(string) error) error {
	name, err := attemptSandboxName(jobName)
	if err != nil {
		return err
	}
	if filepath.Base(evidenceDir) != jobName {
		return fmt.Errorf("evidence directory must match attempt Job")
	}
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return err
	}
	for i := 0; i < 6; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		listed, err := list(ctx)
		if err != nil {
			return err
		}
		if !containsName(listed, name) {
			result := gatewayCleanupResult{Job: jobName, Names: []string{name}, Deleted: []string{name}, CheckedAt: time.Now().UTC()}
			return writeCleanupResult(evidenceDir, result)
		}
		if err := deleteSandbox(name); err != nil {
			return err
		}
		// A successful delete request is not proof of remote absence.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("sandbox %s still exists after cleanup attempts", name)
}

func containsName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func listGatewaySandboxes(ctx context.Context) ([]string, error) {
	if err := refreshGatewayAuth(); err != nil {
		return nil, err
	}
	output, err := exec.CommandContext(ctx, "openshell", "sandbox", "list", "--names", "--page-size", "1000").Output()
	if err != nil {
		return nil, fmt.Errorf("list gateway sandboxes: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(output), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			if !jobNamePattern.MatchString(name) {
				return nil, fmt.Errorf("unexpected gateway sandbox list output")
			}
			names = append(names, name)
		}
	}
	if len(names) >= 1000 {
		return nil, fmt.Errorf("gateway sandbox listing may be truncated; remote absence is unverified")
	}
	return names, nil
}
