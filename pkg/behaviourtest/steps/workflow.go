package steps

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cucumber/godog"
	"gopkg.in/yaml.v3"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/pkg/behaviourtest/world"
)

// Workflow-definition steps (ADR 0130): commit a definition tree from the
// fixtures into the leased repo, point config.yaml workflows: at it, and
// assert on the workflow command the runner records in metrics.json. The
// harness that names the definition is committed with the custom-harness
// step like any other.
func registerWorkflowSteps(sc *godog.ScenarioContext) {
	sc.Step(`^the workflow definition "([^"]+)" is committed at "([^"]+)"$`, func(ctx context.Context, name, dest string) (context.Context, error) {
		return ctx, givenWorkflowDefinitionCommitted(world.FromContext(ctx), name, dest)
	})
	sc.Step(`^the repository workflows are configured with:$`, func(ctx context.Context, doc string) (context.Context, error) {
		return ctx, givenRepositoryWorkflows(world.FromContext(ctx), doc)
	})
	sc.Step(`^the run started the workflow "([^"]+)" from the "([^"]+)" definition at "([^"]+)"$`, func(ctx context.Context, command, definition, source string) (context.Context, error) {
		return ctx, assertRunStartedWorkflow(world.FromContext(ctx), command, definition, source)
	})
}

// workflowFixtureCategory is where definition trees live under the
// fixtures root: fixtures/workflow/<name>/.
var workflowFixtureCategory = filepath.Join("fixtures", "workflow")

// givenWorkflowDefinitionCommitted commits the fixture tree
// fixtures/workflow/<name>/ into the leased repo under dest, one file per
// commit, keeping relative paths. dest must be a sub-directory: the
// leased repo's root is not a plugin, and a definition inside .fullsend/
// is refused by the runner. dest, and each file path under it, must not
// exist yet (a fresh slot has none), so CleanupScenario can delete every
// file the step committed without touching the repository's own; each
// file is recorded in the world before it is written, so a partial
// commit is cleaned up too.
func givenWorkflowDefinitionCommitted(w *world.World, name, dest string) error {
	if w.RepoOwner == "" || w.RepoName == "" {
		return fmt.Errorf("no repo configured; call 'Given the enrolled test repository' before workflow operations")
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("workflow definition fixture %q must be a directory name under %s", name, workflowFixtureCategory)
	}
	repoDir, err := workflowDefinitionRepoDir(dest)
	if err != nil {
		return err
	}
	root, err := moduleRootDir()
	if err != nil {
		return fmt.Errorf("finding module root: %w", err)
	}
	fixturesRoot, err := findModuleSubdir(w.FixturesRoot)
	if err != nil {
		return fmt.Errorf("finding fixtures root: %w", err)
	}
	fixtureDir, err := fixtureSubpath(root, fixturesRoot, workflowFixtureCategory, name)
	if err != nil {
		return fmt.Errorf("workflow definition fixture %q: %w", name, err)
	}
	files, err := fixtureTreeFiles(fixtureDir, repoDir)
	if err != nil {
		return fmt.Errorf("committing workflow definition %q to %s: %w", name, repoDir, err)
	}
	for _, p := range append([]string{repoDir}, fixtureDests(files)...) {
		if err := refuseExistingRepoPath(w, p); err != nil {
			return fmt.Errorf("committing workflow definition %q to %s: %w", name, repoDir, err)
		}
	}
	for _, f := range files {
		w.WorkflowDefinitionFiles = append(w.WorkflowDefinitionFiles, f.dest)
		if err := commitFixtureFile(w, f); err != nil {
			return fmt.Errorf("committing workflow definition %q to %s: %w", name, repoDir, err)
		}
	}
	return nil
}

func fixtureDests(files []fixtureFile) []string {
	dests := make([]string, len(files))
	for i, f := range files {
		dests[i] = f.dest
	}
	return dests
}

// refuseExistingRepoPath fails unless the leased repo has nothing at p.
// A read that fails for any other reason than not found is refused too:
// a directory reads as an error on some forges, and cleanup must never
// delete a file the scenario did not write.
func refuseExistingRepoPath(w *world.World, p string) error {
	_, err := w.SCM.GetFileContent(context.Background(), w.RepoOwner, w.RepoName, p)
	switch {
	case err == nil:
		return fmt.Errorf("%s already exists in the leased repository; a definition is committed only to a path that does not exist yet, so scenario cleanup can delete it; pick another directory", p)
	case forge.IsNotFound(err):
		return nil
	default:
		return fmt.Errorf("cannot tell whether %s exists in the leased repository (%w); a definition is committed only to a path that does not exist yet, so scenario cleanup can delete it; pick another directory", p, err)
	}
}

// RemoveWorkflowDefinitionFiles deletes every file
// givenWorkflowDefinitionCommitted recorded, so the next scenario on this
// slot starts without the definition. A file that is not there (its
// commit failed) is skipped. Exported so CleanupScenario can call it
// during scenario teardown.
func RemoveWorkflowDefinitionFiles(w *world.World) {
	ctx := context.Background()
	for _, p := range w.WorkflowDefinitionFiles {
		desc := fmt.Sprintf("delete workflow definition file %s", p)
		if err := cleanupRetry(w.Logf, desc, func() error {
			err := w.SCM.DeleteFile(ctx, w.RepoOwner, w.RepoName, p, "behaviour: remove workflow definition file "+p)
			if forge.IsNotFound(err) {
				return nil
			}
			return err
		}); err != nil {
			worldLogf(w, "behaviour cleanup: %s: %v", desc, err)
		}
	}
	w.WorkflowDefinitionFiles = nil
}

// workflowDefinitionRepoDir checks the repository directory a definition
// is committed to and returns it cleaned.
func workflowDefinitionRepoDir(dest string) (string, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" || strings.Contains(dest, `\`) || path.IsAbs(dest) {
		return "", fmt.Errorf("workflow definition directory %q must be a relative path with / separators", dest)
	}
	clean := path.Clean(dest)
	if clean == "." {
		return "", fmt.Errorf("workflow definition directory %q is the repository root, which would make the whole repository the definition; use a sub-directory", dest)
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", fmt.Errorf("workflow definition directory %q leaves the repository", dest)
		}
	}
	if clean == ".fullsend" || strings.HasPrefix(clean, ".fullsend/") {
		return "", fmt.Errorf("workflow definition directory %q is inside .fullsend/, which the runner refuses as a source; use a directory outside it", dest)
	}
	return clean, nil
}

// givenRepositoryWorkflows commits workflows: entries (a YAML list of
// name/source mappings) into the enrolled repo's .fullsend/config.yaml —
// replacing the entry of the same name, compared without case, else
// appending — and records the pre-scenario list so CleanupScenario
// restores it. The list is validated the way `fullsend run` validates
// it, so a scenario cannot commit a config the runner would refuse.
func givenRepositoryWorkflows(w *world.World, doc string) error {
	if w.Org == "" || w.RepoName == "" {
		return fmt.Errorf("no repo configured; call 'Given the enrolled test repository' before workflow operations")
	}
	var entries []config.WorkflowEntry
	if err := yaml.Unmarshal([]byte(doc), &entries); err != nil {
		return fmt.Errorf("parsing workflows docstring: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("workflows docstring must hold at least one entry")
	}
	if err := snapshotWorkflows(w); err != nil {
		return err
	}
	cfgPath := filepath.Join(".fullsend", "config.yaml")
	cfg, err := readPerRepoConfig(w, cfgPath)
	if err != nil {
		return err
	}
	cfg.SetWorkflows(upsertWorkflowEntries(cfg.WorkflowEntries(), entries))
	if err := config.ValidateWorkflowEntries(cfg.WorkflowEntries(), cfg.AllowedResources()); err != nil {
		return fmt.Errorf("workflows: %w", err)
	}
	merged, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := w.SCM.CommitFile(context.Background(), w.Org, w.RepoName, cfgPath, "behaviour: set workflows", merged); err != nil {
		return fmt.Errorf("updating config: %w", err)
	}
	return nil
}

// upsertWorkflowEntries returns current with each of updates replacing the
// entry of the same name (compared without case) or appended. current is
// not modified.
func upsertWorkflowEntries(current, updates []config.WorkflowEntry) []config.WorkflowEntry {
	out := make([]config.WorkflowEntry, len(current), len(current)+len(updates))
	copy(out, current)
	for _, u := range updates {
		replaced := false
		for i := range out {
			if strings.EqualFold(out[i].Name, u.Name) {
				out[i] = u
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, u)
		}
	}
	return out
}

// snapshotWorkflows records the config.yaml workflows: list once per
// scenario so CleanupScenario can restore it.
func snapshotWorkflows(w *world.World) error {
	if w.WorkflowsOverridden {
		return nil
	}
	cfg, err := readPerRepoConfig(w, filepath.Join(".fullsend", "config.yaml"))
	if err != nil {
		return fmt.Errorf("workflows snapshot: %w", err)
	}
	if orig := cfg.WorkflowEntries(); orig != nil {
		// Store a copy so later mutations do not alias the snapshot.
		w.WorkflowsOriginal = append([]config.WorkflowEntry(nil), orig...)
	}
	w.WorkflowsOverridden = true
	return nil
}

// RestoreWorkflows sets config.yaml workflows: back to the pre-scenario
// value captured by snapshotWorkflows (dropping the key when it was
// absent). Exported so CleanupScenario can call it during scenario
// teardown.
func RestoreWorkflows(w *world.World) error {
	if w.Org == "" || w.RepoName == "" {
		return fmt.Errorf("no repo configured; call 'Given the enrolled test repository' before workflow operations")
	}
	cfgPath := filepath.Join(".fullsend", "config.yaml")
	cfg, err := readPerRepoConfig(w, cfgPath)
	if err != nil {
		return err
	}
	cfg.SetWorkflows(w.WorkflowsOriginal)
	merged, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := w.SCM.CommitFile(context.Background(), w.Org, w.RepoName, cfgPath, "behaviour: restore workflows", merged); err != nil {
		return fmt.Errorf("updating config: %w", err)
	}
	return nil
}

// treeHashPattern is the form of metrics.json workflow.pin_sha256: the
// definition's full sha256 tree hash.
var treeHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// assertRunStartedWorkflow checks the workflow object the runner writes
// into metrics.json on every runtime: the command it started, the
// workflows: entry it came from and that entry's source, and a full tree
// hash. Under the dummy runtime no model runs, so this is the end-to-end
// proof that the harness workflow: field was resolved against config.yaml
// and the definition was read and hashed.
func assertRunStartedWorkflow(w *world.World, command, definition, source string) error {
	m, err := readRunMetrics(w)
	if err != nil {
		return err
	}
	if m.Workflow == nil {
		return fmt.Errorf("metrics.json has no workflow object; the run started no workflow (want %q)", command)
	}
	got := *m.Workflow
	if got.Command != command {
		return fmt.Errorf("metrics.json workflow.command = %q, want %q", got.Command, command)
	}
	if got.Definition != definition {
		return fmt.Errorf("metrics.json workflow.definition = %q, want %q", got.Definition, definition)
	}
	if got.Source != source {
		return fmt.Errorf("metrics.json workflow.source = %q, want %q", got.Source, source)
	}
	if !treeHashPattern.MatchString(got.PinSHA256) {
		return fmt.Errorf("metrics.json workflow.pin_sha256 = %q, want 64 lower-case hex characters", got.PinSHA256)
	}
	return nil
}
