package k8spoc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/fullsend-ai/fullsend/internal/sandbox"
)

var probeJobPattern = regexp.MustCompile(`^fs-gateway-([0-9a-f]{8})$`)
var demoJobPattern = regexp.MustCompile(`^fs-demo-([0-9a-f]{8})$`)

func ownedGatewayNames(job string) ([]string, bool) {
	if matches := probeJobPattern.FindStringSubmatch(job); matches != nil {
		return []string{"fs-" + matches[1] + "a", "fs-" + matches[1] + "b"}, true
	}
	if matches := demoJobPattern.FindStringSubmatch(job); matches != nil {
		return []string{"fs-" + matches[1]}, true
	}
	return nil, false
}

type gatewayCleanupResult struct {
	Job       string    `json:"job"`
	Names     []string  `json:"names"`
	Deleted   []string  `json:"deleted,omitempty"`
	Errors    []string  `json:"errors,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// GatewayCleanup finds remote sandboxes whose probes did not record deletion.
// It derives names from owned Job directories rather than trusting names in
// runner-written files. The evidence PVC remains available after the Pod dies.
func GatewayCleanup(ctx context.Context, evidenceDir string) error {
	if evidenceDir == "" {
		return fmt.Errorf("evidence directory is required")
	}
	if err := sandbox.EnsureAvailable(); err != nil {
		return err
	}
	if err := refreshGatewayAuth(); err != nil {
		return err
	}
	if err := sandbox.CheckGateway(); err != nil {
		return err
	}
	return gatewayCleanupWithDelete(ctx, evidenceDir, deleteGatewaySandbox)
}

func gatewayCleanupWithDelete(ctx context.Context, evidenceDir string, deleteSandbox func(string) error) error {
	entries, err := os.ReadDir(evidenceDir)
	if err != nil {
		return err
	}
	var problems []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		derived, owned := ownedGatewayNames(entry.Name())
		if !owned {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(problems, err)...)
		}
		dir := filepath.Join(evidenceDir, entry.Name())
		intent, err := readProbeResult(filepath.Join(dir, "intent.json"))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s intent: %w", entry.Name(), err))
			continue
		}
		if !slices.Equal(intent.Names, derived) {
			problems = append(problems, fmt.Errorf("%s intent names do not match owned Job", entry.Name()))
			continue
		}
		result, err := readProbeResult(filepath.Join(dir, "result.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("%s result: %w", entry.Name(), err))
			continue
		}
		previous, err := readCleanupResult(filepath.Join(dir, "cleanup.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("%s cleanup: %w", entry.Name(), err))
			continue
		}
		cleanup := gatewayCleanupResult{Job: entry.Name(), Names: derived, CheckedAt: time.Now().UTC()}
		for _, name := range derived {
			if slices.Contains(result.Deleted, name) || slices.Contains(previous.Deleted, name) {
				cleanup.Deleted = append(cleanup.Deleted, name)
				continue
			}
			if err := deleteSandbox(name); err != nil {
				cleanup.Errors = append(cleanup.Errors, fmt.Sprintf("%s: %v", name, err))
				problems = append(problems, fmt.Errorf("%s delete %s: %w", entry.Name(), name, err))
			} else {
				cleanup.Deleted = append(cleanup.Deleted, name)
			}
		}
		if err := writeCleanupResult(dir, cleanup); err != nil {
			problems = append(problems, err)
		} else if summary, err := json.Marshal(cleanup); err == nil {
			fmt.Println(string(summary))
		}
	}
	return errors.Join(problems...)
}

func readProbeResult(path string) (gatewayProbeResult, error) {
	var result gatewayProbeResult
	b, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(b, &result)
	return result, err
}

func writeCleanupResult(dir string, result gatewayCleanupResult) error {
	return writeEvidenceJSON(dir, "cleanup.json", result)
}

func readCleanupResult(path string) (gatewayCleanupResult, error) {
	var result gatewayCleanupResult
	b, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(b, &result)
	return result, err
}
