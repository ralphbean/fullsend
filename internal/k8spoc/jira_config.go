package k8spoc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/harnessdispatch"
	"github.com/fullsend-ai/fullsend/internal/repos"
)

// PrepareLiveConfig validates a repository registration and snapshots its
// current CEL harnesses for a bounded Jira source. The resulting file is
// mounted read-only in the control plane and stored by digest in PostgreSQL.
func PrepareLiveConfig(ctx context.Context, manifestPath, repoDir, repoSlug, jiraURL, project, label, outputPath string) error {
	owner, name, ok := strings.Cut(repoSlug, "/")
	if !ok || !repoPattern.MatchString(repoSlug) {
		return fmt.Errorf("repo must be an owner/repo slug")
	}
	manifest, err := repos.LoadManifest(ctx, manifestPath)
	if err != nil {
		return err
	}
	if err = manifest.Validate(); err != nil {
		return err
	}
	if _, found := manifest.ResolveConfig(owner, name); !found {
		return fmt.Errorf("repository %s is not registered in repos manifest", repoSlug)
	}
	configDir := filepath.Join(repoDir, ".fullsend")
	cfg, err := config.LoadConfig(configDir, config.LoadOpts{MissingOK: false})
	if err != nil {
		return err
	}
	candidates, err := harnessdispatch.ListTriggeredHarnessesStrict(ctx, configDir, cfg, nil)
	if err != nil {
		return err
	}
	selection := LiveConfig{JiraBaseURL: jiraURL, Project: project, Label: label, Repo: repoSlug}
	selection.FullsendHarnesses = splitConfigList(os.Getenv("K8S_POC_FULLSEND_HARNESSES"))
	selection.RepoBundle = os.Getenv("K8S_POC_REPO_BUNDLE")
	selection.RepoRevision = os.Getenv("K8S_POC_REPO_REVISION")
	selection.GCPAudience = os.Getenv("K8S_POC_GCP_AUDIENCE")
	selection.VertexProject = os.Getenv("K8S_POC_VERTEX_PROJECT")
	for _, candidate := range candidates {
		selection.Harnesses = append(selection.Harnesses, LiveHarness{
			Name: candidate.Name, Role: candidate.Harness.Role, Trigger: candidate.Harness.Trigger,
		})
	}
	if err = selection.validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outputPath, append(b, '\n'), 0o600)
}

func splitConfigList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
