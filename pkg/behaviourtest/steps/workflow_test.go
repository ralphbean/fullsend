package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/resolve"
	"github.com/fullsend-ai/fullsend/pkg/behaviourtest/world"
)

// treeSCM is a leased repository held in memory: it records every file
// committed, by path, reads them back, and deletes them. failAfter > 0
// makes the commit after that many succeeded fail.
type treeSCM struct {
	fakeCleanupSCM
	files     map[string][]byte
	commits   int
	failAfter int
}

func (s *treeSCM) CommitFile(_ context.Context, _, _, path, _ string, content []byte) error {
	if s.failAfter > 0 && s.commits >= s.failAfter {
		return errors.New("commit refused")
	}
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = content
	s.commits++
	return nil
}

func (s *treeSCM) GetFileContent(_ context.Context, _, _, path string) ([]byte, error) {
	if c, ok := s.files[path]; ok {
		return c, nil
	}
	return nil, forge.ErrNotFound
}

func (s *treeSCM) DeleteFile(_ context.Context, _, _, path, _ string) error {
	if _, ok := s.files[path]; !ok {
		return forge.ErrNotFound
	}
	delete(s.files, path)
	s.deletedFiles = append(s.deletedFiles, path)
	return nil
}

func TestWorkflowDefinitionRepoDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		dest, want, err string
	}{
		{dest: "pipelines/sample-pipeline", want: "pipelines/sample-pipeline"},
		{dest: "pipelines/./sample/", want: "pipelines/sample"},
		{dest: ".", err: "repository root"},
		{dest: "", err: "relative path"},
		{dest: "/abs", err: "relative path"},
		{dest: `a\b`, err: "relative path"},
		{dest: "../out", err: "leaves the repository"},
		{dest: ".fullsend/pipeline", err: "inside .fullsend/"},
		{dest: ".fullsend", err: "inside .fullsend/"},
	} {
		got, err := workflowDefinitionRepoDir(tc.dest)
		if tc.err != "" {
			require.ErrorContains(t, err, tc.err, tc.dest)
			continue
		}
		require.NoError(t, err, tc.dest)
		assert.Equal(t, tc.want, got)
	}
}

func TestGivenWorkflowDefinitionCommitted(t *testing.T) {
	t.Parallel()
	scm := &treeSCM{}
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm, FixturesRoot: "e2e/behaviour"}

	require.NoError(t, givenWorkflowDefinitionCommitted(w, "sample-pipeline", "pipelines/sample-pipeline"))
	paths := make([]string, 0, len(scm.files))
	for p := range scm.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	assert.Equal(t, []string{
		"pipelines/sample-pipeline/.claude-plugin/plugin.json",
		"pipelines/sample-pipeline/scripts/checklist.sh",
		"pipelines/sample-pipeline/skills/read-issue/SKILL.md",
		"pipelines/sample-pipeline/workflows/triage-fanout.js",
	}, paths)

	require.ErrorContains(t, givenWorkflowDefinitionCommitted(w, "../workflow", "x"), "directory name")
	require.ErrorContains(t, givenWorkflowDefinitionCommitted(w, "sample-pipeline", "."), "repository root")
	require.ErrorContains(t, givenWorkflowDefinitionCommitted(&world.World{}, "sample-pipeline", "x"), "no repo configured")
	require.Error(t, givenWorkflowDefinitionCommitted(w, "missing", "x"))
}

var sampleDefinitionFiles = []string{
	"pipelines/sample-pipeline/.claude-plugin/plugin.json",
	"pipelines/sample-pipeline/scripts/checklist.sh",
	"pipelines/sample-pipeline/skills/read-issue/SKILL.md",
	"pipelines/sample-pipeline/workflows/triage-fanout.js",
}

// A slot whose repository already has the destination, or any file the
// definition would write, is refused before anything is committed, so
// cleanup never deletes a file the scenario did not write.
func TestGivenWorkflowDefinitionCommitted_ExistingDestinationRefused(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"pipelines/sample-pipeline", "pipelines/sample-pipeline/workflows/triage-fanout.js"} {
		scm := &treeSCM{files: map[string][]byte{existing: []byte("theirs")}}
		w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm, FixturesRoot: "e2e/behaviour"}
		err := givenWorkflowDefinitionCommitted(w, "sample-pipeline", "pipelines/sample-pipeline")
		require.ErrorContains(t, err, existing+" already exists in the leased repository")
		assert.Equal(t, 0, scm.commits, existing)
		assert.Empty(t, w.WorkflowDefinitionFiles, existing)
	}

	scm := &treeSCM{}
	scm.getFileErr = errors.New("decode file content: json: cannot unmarshal array")
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: &unreadableSCM{scm}, FixturesRoot: "e2e/behaviour"}
	err := givenWorkflowDefinitionCommitted(w, "sample-pipeline", "pipelines/sample-pipeline")
	require.ErrorContains(t, err, "cannot tell whether pipelines/sample-pipeline exists")
	assert.Equal(t, 0, scm.commits)
}

// unreadableSCM fails every read with the embedded fake's getFileErr.
type unreadableSCM struct{ *treeSCM }

func (s *unreadableSCM) GetFileContent(context.Context, string, string, string) ([]byte, error) {
	return nil, s.getFileErr
}

// CleanupScenario deletes every definition file the step committed.
func TestCleanupScenario_RemovesWorkflowDefinition(t *testing.T) {
	t.Parallel()
	scm := &treeSCM{}
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm, FixturesRoot: "e2e/behaviour"}
	require.NoError(t, givenWorkflowDefinitionCommitted(w, "sample-pipeline", "pipelines/sample-pipeline"))
	assert.ElementsMatch(t, sampleDefinitionFiles, w.WorkflowDefinitionFiles)

	CleanupScenario(w)
	assert.Empty(t, scm.files, "every committed definition file is deleted")
	assert.ElementsMatch(t, sampleDefinitionFiles, scm.deletedFiles)
	assert.Empty(t, w.WorkflowDefinitionFiles)
}

// A commit that fails partway leaves the files written so far, and the
// one that failed, recorded; cleanup deletes the written ones and skips
// the missing one.
func TestCleanupScenario_RemovesPartialWorkflowDefinition(t *testing.T) {
	t.Parallel()
	scm := &treeSCM{failAfter: 2}
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm, FixturesRoot: "e2e/behaviour"}
	err := givenWorkflowDefinitionCommitted(w, "sample-pipeline", "pipelines/sample-pipeline")
	require.ErrorContains(t, err, "commit refused")
	require.Len(t, scm.files, 2)
	require.Len(t, w.WorkflowDefinitionFiles, 3, "the failed file is recorded before its write")

	CleanupScenario(w)
	assert.Empty(t, scm.files)
	assert.Len(t, scm.deletedFiles, 2)
}

// TestSampleWorkflowFixtureResolves runs the fixture through the runner's
// own resolution, as a local workflows: source in a git checkout laid out
// like the leased repo, so a fixture the runner would refuse fails here
// rather than only on a live run.
func TestSampleWorkflowFixtureResolves(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := moduleRootDir()
	require.NoError(t, err)
	src := filepath.Join(root, "e2e", "behaviour", "fixtures", "workflow", "sample-pipeline")

	repo := t.TempDir()
	def := filepath.Join(repo, "pipelines", "sample-pipeline")
	require.NoError(t, filepath.WalkDir(src, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dest := filepath.Join(def, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	}))
	fullsendDir := filepath.Join(repo, ".fullsend")
	require.NoError(t, os.MkdirAll(fullsendDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fullsendDir, "config.yaml"), []byte("version: \"1\"\n"), 0o644))
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	}

	h := &harness.Harness{Workflow: &harness.WorkflowSpec{Definition: "sample-pipeline", Name: "triage-fanout", Args: "issue 1"}}
	rw, dep, err := resolve.ResolveWorkflowDefinition(context.Background(), h,
		[]config.WorkflowEntry{{Name: "sample-pipeline", Source: "pipelines/sample-pipeline"}}, fullsendDir,
		resolve.ResolveOpts{WorkspaceRoot: fullsendDir})
	require.NoError(t, err)
	assert.Nil(t, dep)
	assert.Equal(t, "/sample-pipeline:triage-fanout issue 1", rw.Command())
	assert.Regexp(t, treeHashPattern, rw.TreeHash)
}

func TestUpsertWorkflowEntries(t *testing.T) {
	t.Parallel()
	current := []config.WorkflowEntry{{Name: "a", Source: "x"}, {Name: "B", Source: "y"}}
	got := upsertWorkflowEntries(current, []config.WorkflowEntry{{Name: "b", Source: "z"}, {Name: "c", Source: "w"}})
	assert.Equal(t, []config.WorkflowEntry{{Name: "a", Source: "x"}, {Name: "b", Source: "z"}, {Name: "c", Source: "w"}}, got)
	assert.Equal(t, "y", current[1].Source, "current is not modified")
}

func TestGivenRepositoryWorkflows_WritesValidatesAndRestores(t *testing.T) {
	t.Parallel()
	scm := &recordingSCM{fakeCleanupSCM: fakeCleanupSCM{fileContent: []byte(perRepoDummyConfig)}}
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm}

	require.NoError(t, givenRepositoryWorkflows(w, "- name: sample-pipeline\n  source: pipelines/sample-pipeline\n"))
	assert.True(t, w.WorkflowsOverridden)
	assert.Nil(t, w.WorkflowsOriginal, "no workflows: key before the scenario")
	assert.Equal(t, filepath.Join(".fullsend", "config.yaml"), scm.lastPath)
	cfg, err := config.ParsePerRepoConfigWriter(scm.lastContent)
	require.NoError(t, err)
	assert.Equal(t, []config.WorkflowEntry{{Name: "sample-pipeline", Source: "pipelines/sample-pipeline"}}, cfg.WorkflowEntries())
	assert.Equal(t, "dummy", cfg.ConfigRuntime(), "the rest of the config is kept")

	// A second step replaces by name and keeps the first snapshot.
	scm.fileContent = scm.lastContent
	require.NoError(t, givenRepositoryWorkflows(w, "- name: SAMPLE-pipeline\n  source: other\n"))
	cfg, err = config.ParsePerRepoConfigWriter(scm.lastContent)
	require.NoError(t, err)
	assert.Equal(t, []config.WorkflowEntry{{Name: "SAMPLE-pipeline", Source: "other"}}, cfg.WorkflowEntries())
	assert.Nil(t, w.WorkflowsOriginal)

	scm.fileContent = scm.lastContent
	require.NoError(t, RestoreWorkflows(w))
	assert.NotContains(t, string(scm.lastContent), "workflows:")
}

func TestGivenRepositoryWorkflows_Refusals(t *testing.T) {
	t.Parallel()
	newWorld := func() *world.World {
		scm := &recordingSCM{fakeCleanupSCM: fakeCleanupSCM{fileContent: []byte(perRepoDummyConfig)}}
		return &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm}
	}
	require.ErrorContains(t, givenRepositoryWorkflows(newWorld(), "[]"), "at least one entry")
	require.ErrorContains(t, givenRepositoryWorkflows(newWorld(), "name: x"), "parsing workflows docstring")
	require.ErrorContains(t, givenRepositoryWorkflows(newWorld(), "- name: a/b\n  source: x\n"), "workflows:")
	require.ErrorContains(t, givenRepositoryWorkflows(&world.World{}, "- name: a\n  source: x\n"), "no repo configured")
	require.ErrorContains(t, RestoreWorkflows(&world.World{}), "no repo configured")
}

func TestCleanupScenario_RestoresWorkflows(t *testing.T) {
	t.Parallel()
	scm := &recordingSCM{fakeCleanupSCM: fakeCleanupSCM{fileContent: []byte(perRepoDummyConfig + "workflows:\n  - name: sample\n    source: x\n")}}
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm, WorkflowsOverridden: true}
	CleanupScenario(w)
	assert.Equal(t, filepath.Join(".fullsend", "config.yaml"), scm.lastPath)
	assert.NotContains(t, string(scm.lastContent), "workflows:")
}

func TestAssertRunStartedWorkflow(t *testing.T) {
	t.Parallel()
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	root := t.TempDir()
	writeArtifact(t, root, "agent-workflow-triage-1/metrics.json",
		`{"runtime":"dummy","workflow":{"definition":"sample-pipeline","source":"pipelines/sample-pipeline","pin_sha256":"`+hash+`","command":"/sample-pipeline:triage-fanout issue 1"}}`)
	w := &world.World{ArtifactDir: root}

	require.NoError(t, assertRunStartedWorkflow(w, "/sample-pipeline:triage-fanout issue 1", "sample-pipeline", "pipelines/sample-pipeline"))
	require.ErrorContains(t, assertRunStartedWorkflow(w, "/sample-pipeline:other", "sample-pipeline", "pipelines/sample-pipeline"), "workflow.command")
	require.ErrorContains(t, assertRunStartedWorkflow(w, "/sample-pipeline:triage-fanout issue 1", "other", "pipelines/sample-pipeline"), "workflow.definition")
	require.ErrorContains(t, assertRunStartedWorkflow(w, "/sample-pipeline:triage-fanout issue 1", "sample-pipeline", "."), "workflow.source")

	short := t.TempDir()
	writeArtifact(t, short, "metrics.json", `{"workflow":{"definition":"d","source":"s","pin_sha256":"abc","command":"/d:n"}}`)
	require.ErrorContains(t, assertRunStartedWorkflow(&world.World{ArtifactDir: short}, "/d:n", "d", "s"), "pin_sha256")

	none := t.TempDir()
	writeArtifact(t, none, "metrics.json", `{"runtime":"dummy"}`)
	require.ErrorContains(t, assertRunStartedWorkflow(&world.World{ArtifactDir: none}, "/d:n", "d", "s"), "no workflow object")
}

// Forge failures while reading or writing config.yaml surface as step
// errors.
func TestGivenRepositoryWorkflows_ForgeErrors(t *testing.T) {
	t.Parallel()
	readFails := &recordingSCM{fakeCleanupSCM: fakeCleanupSCM{getFileErr: errors.New("read refused")}}
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: readFails}
	require.ErrorContains(t, givenRepositoryWorkflows(w, "- name: a\n  source: x\n"), "workflows snapshot: reading config: read refused")
	require.ErrorContains(t, RestoreWorkflows(w), "reading config: read refused")

	writeFails := &recordingSCM{fakeCleanupSCM: fakeCleanupSCM{fileContent: []byte(perRepoDummyConfig), commitFileErr: errors.New("write refused")}}
	w = &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: writeFails}
	require.ErrorContains(t, givenRepositoryWorkflows(w, "- name: a\n  source: x\n"), "updating config: write refused")
	require.ErrorContains(t, RestoreWorkflows(w), "updating config: write refused")
}

// A delete that fails for another reason than not found is logged, and
// the recorded list is cleared either way.
func TestRemoveWorkflowDefinitionFiles_DeleteError(t *testing.T) {
	t.Parallel()
	scm := &fakeCleanupSCM{deleteFileErr: errors.New("delete refused")}
	var logged []string
	w := &world.World{Org: "org", RepoOwner: "org", RepoName: "repo", SCM: scm,
		WorkflowDefinitionFiles: []string{"pipelines/x/a", "pipelines/x/b"},
		Logf:                    func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
	RemoveWorkflowDefinitionFiles(w)
	assert.Equal(t, []string{"pipelines/x/a", "pipelines/x/b"}, scm.deletedFiles)
	assert.Empty(t, w.WorkflowDefinitionFiles)
	require.Len(t, logged, 2)
	assert.Contains(t, logged[0], "delete workflow definition file pipelines/x/a: delete refused")
}

func TestAssertRunStartedWorkflow_NoMetrics(t *testing.T) {
	t.Parallel()
	require.Error(t, assertRunStartedWorkflow(&world.World{ArtifactDir: t.TempDir()}, "/a:b", "a", "x"))
}
