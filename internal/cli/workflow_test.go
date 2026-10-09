package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/fetch"
	"github.com/fullsend-ai/fullsend/internal/gitfetch"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/lock"
	"github.com/fullsend-ai/fullsend/internal/resolve"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

const cliWorkflowSHA = "0123456789abcdef0123456789abcdef01234567"

func cliWorkflowTree() map[string][]byte {
	return map[string][]byte{
		".claude-plugin/plugin.json": []byte(`{"name":"wfplug"}`),
		"workflows/probe.js":         []byte("export const meta = { name: 'probe' };\n"),
	}
}

func TestCheckWorkflowRuntime(t *testing.T) {
	wf := &harness.Harness{Workflow: &harness.WorkflowSpec{Definition: "d", Name: "n"}}
	tests := []struct {
		name    string
		h       *harness.Harness
		runtime string
		wantErr string
	}{
		{name: "no workflow under pi", h: &harness.Harness{}, runtime: "pi"},
		{name: "claude", h: wf, runtime: "claude"},
		{name: "dummy", h: wf, runtime: "dummy"},
		{name: "dummy-playback", h: wf, runtime: "dummy-playback"},
		{name: "pi", h: wf, runtime: "pi", wantErr: `workflow: needs the claude runtime; agent "code" resolves to "pi"`},
		{name: "codex", h: wf, runtime: "codex", wantErr: `workflow: needs the claude runtime; agent "code" resolves to "codex"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkWorkflowRuntime(tt.h, "code", tt.runtime)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestDescribeWorkflow(t *testing.T) {
	hash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	tests := []struct {
		name string
		rw   resolve.ResolvedWorkflow
		want string
	}{
		{
			name: "remote with args",
			rw:   resolve.ResolvedWorkflow{Definition: "sample-pipeline", Name: "probe", Args: "--issue 7", Commit: cliWorkflowSHA, TreeHash: hash, PluginName: "wfplug"},
			want: "sample-pipeline (0123456789ab, sha256:abcdef012345) → /wfplug:probe --issue 7",
		},
		{
			name: "local without args",
			rw:   resolve.ResolvedWorkflow{Definition: "self", Name: "probe", Local: true, Source: ".", TreeHash: hash, PluginName: "wfplug"},
			want: "self (local ., sha256:abcdef012345) → /wfplug:probe",
		},
		{
			name: "short values are kept",
			rw:   resolve.ResolvedWorkflow{Definition: "d", Name: "n", Commit: "abc", TreeHash: "def", PluginName: "p"},
			want: "d (abc, sha256:def) → /p:n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, describeWorkflow(&tt.rw))
		})
	}
}

func TestResolveHarnessWorkflow(t *testing.T) {
	t.Run("no workflow", func(t *testing.T) {
		rw, dep, err := resolveHarnessWorkflow(context.Background(), &harness.Harness{}, nil, t.TempDir(), resolve.ResolveOpts{})
		require.NoError(t, err)
		assert.Nil(t, rw)
		assert.Nil(t, dep)
	})
	t.Run("no config", func(t *testing.T) {
		h := &harness.Harness{Workflow: &harness.WorkflowSpec{Definition: "sample", Name: "probe"}}
		_, _, err := resolveHarnessWorkflow(context.Background(), h, nil, t.TempDir(), resolve.ResolveOpts{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config.yaml has no workflows: entries")
	})
	t.Run("local source resolves against the repository root", func(t *testing.T) {
		repo := t.TempDir()
		fullsendDir := filepath.Join(repo, ".fullsend")
		require.NoError(t, os.MkdirAll(fullsendDir, 0o755))
		for p, c := range cliWorkflowTree() {
			full := filepath.Join(repo, "def", filepath.FromSlash(p))
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, c, 0o644))
		}
		require.NoError(t, os.WriteFile(filepath.Join(fullsendDir, "config.yaml"), []byte("workflows:\n  - name: self\n    source: def\n"), 0o644))
		gitInit(t, repo)
		cfg, err := loadLockConfig(filepath.Join(fullsendDir, "config.yaml"))
		require.NoError(t, err)
		h := &harness.Harness{Workflow: &harness.WorkflowSpec{Definition: "self", Name: "probe"}}
		rw, dep, err := resolveHarnessWorkflow(context.Background(), h, cfg, fullsendDir, resolve.ResolveOpts{WorkspaceRoot: fullsendDir})
		require.NoError(t, err)
		assert.Nil(t, dep)
		assert.Equal(t, "self (local def, sha256:"+rw.TreeHash[:12]+") → /wfplug:probe", describeWorkflow(rw))
	})
}

func TestRunLock_WorkflowDefinition(t *testing.T) {
	files := cliWorkflowTree()
	treeHash := fetch.ComputeTreeHash(files)
	url := "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "harness"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "code.yaml"), []byte(`agent: agents/code.md
role: test
workflow:
  definition: sample-pipeline
  name: probe
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`allowed_remote_resources:
  - https://github.com/example-org/
workflows:
  - name: sample-pipeline
    source: `+url+`#sha256=`+treeHash+`
`), 0o644))

	fetcher := func(context.Context, string, string, string, string) (map[string][]byte, error) {
		return files, nil
	}
	printer := ui.New(os.Stdout)
	require.NoError(t, runLock(context.Background(), "code", dir, "", false, resolveFlags{treeFetcher: gitfetch.TreeFetchFunc(fetcher)}, printer))

	lf, err := lock.Load(filepath.Join(dir, "lock.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 2, lf.Version, "a lock file that records a workflow definition is version 2, which older releases refuse")
	entry := lf.Lookup("code")
	require.NotNil(t, entry)
	require.Len(t, entry.Dependencies, 1)
	dep := entry.Dependencies[0]
	assert.Equal(t, "workflow", dep.Field)
	assert.Equal(t, url, dep.URL)
	assert.Equal(t, "directory", dep.Type)
	assert.Equal(t, treeHash, dep.SHA256)
	assert.Len(t, dep.Files, 2)

	// Replaying the lock verifies the cache and leaves skills and
	// plugins alone: the definition is delivered by its own resolver.
	h, err := harness.Load(filepath.Join(dir, "harness", "code.yaml"))
	require.NoError(t, err)
	require.NoError(t, h.ResolveRelativeTo(dir))
	result, err := resolveFromLock(h, entry, dir, []string{"https://github.com/example-org/"}, printer)
	require.NoError(t, err)
	require.Len(t, result.Deps, 1)
	assert.Empty(t, h.Skills)
	assert.Empty(t, h.Plugins)
}

func TestRunLock_WorkflowDefinitionError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "harness"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "code.yaml"), []byte(`agent: agents/code.md
role: test
workflow:
  definition: missing
  name: probe
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("version: \"1\"\n"), 0o644))
	err := runLock(context.Background(), "code", dir, "", false, resolveFlags{}, ui.New(os.Stdout))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workflow.definition "missing": config.yaml has no workflows: entries`)
}

// newWorkflowRunDir is newSkipHarnessDir with a workflow: harness and a
// local definition at <fullsend dir>/wfdef. The fullsend dir is the git
// checkout itself, as for an org configuration repository, so the source
// is wfdef.
func newWorkflowRunDir(t *testing.T, runtimeLine string) string {
	t.Helper()
	dir := newSkipHarnessDir(t, "")
	for p, c := range cliWorkflowTree() {
		full := filepath.Join(dir, "wfdef", filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, c, 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "code.yaml"),
		[]byte("agent: agents/code.md\nrole: test\nworkflow:\n  definition: sample-pipeline\n  name: probe\n  args: --issue 7\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(runtimeLine+
		"agents:\n  - harness/code.yaml\nworkflows:\n  - name: sample-pipeline\n    source: wfdef\n"), 0o644))
	gitInit(t, dir)
	return dir
}

func TestRunAgent_WorkflowPlanLine(t *testing.T) {
	usePreScriptStub(t)
	dir := newWorkflowRunDir(t, "")

	var buf bytes.Buffer
	err := runAgent(context.Background(), "code", dir, "", t.TempDir(), "", nil, false, "", "", "", resolveFlags{maxDepth: 10, maxResources: 50},
		statusOpts{}, ui.New(&buf), false, runOverrideFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating sandbox", "the workflow resolved and the run reached sandbox creation")
	assert.Contains(t, buf.String(), "sample-pipeline (local wfdef, sha256:")
	assert.Contains(t, buf.String(), ") → /wfplug:probe --issue 7")
	assert.NotContains(t, buf.String(), "delivered as plugin")
	assert.Contains(t, buf.String(), "sample-pipeline (claude)", "the definition is delivered as a Claude plugin")
}

// The tools check runs at plan time, before the sandbox: an agent whose
// tools: list leaves out Workflow cannot start the command.
func TestRunAgent_WorkflowAgentToolsWithoutWorkflow(t *testing.T) {
	usePreScriptStub(t)
	dir := newWorkflowRunDir(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agents", "code.md"),
		[]byte("---\nname: code\ntools: Read, Bash(gh)\n---\nYou are a coding agent.\n"), 0o644))

	var buf bytes.Buffer
	err := runAgent(context.Background(), "code", dir, "", t.TempDir(), "", nil, false, "", "", "", resolveFlags{maxDepth: 10, maxResources: 50},
		statusOpts{}, ui.New(&buf), false, runOverrideFlags{})
	require.Error(t, err)
	assert.Equal(t, "workflow: agent code lists tools: without Workflow, so it cannot start /wfplug:probe; add Workflow to its tools:", err.Error())
	assert.NotContains(t, err.Error(), "creating sandbox")
}

func TestCheckWorkflowAgentTools(t *testing.T) {
	dir := t.TempDir()
	agent := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}
	rw := &resolve.ResolvedWorkflow{Definition: "sample-pipeline", Name: "probe", Args: "--issue 7", PluginName: "wfplug"}
	tests := []struct {
		name    string
		agent   string
		rw      *resolve.ResolvedWorkflow
		wantErr string
	}{
		{name: "tools: names Workflow", agent: agent("with.md", "---\nname: a\ntools: Read, Workflow\n---\nbody"), rw: rw},
		{name: "tools: names Workflow with arguments", agent: agent("args.md", "---\nname: a\ntools:\n  - Workflow(wfplug:probe)\n---\nbody"), rw: rw},
		{name: "no tools: inherits every tool", agent: agent("none.md", "---\nname: a\n---\nbody"), rw: rw},
		{name: "no workflow", agent: agent("no-wf.md", "---\nname: a\ntools: Read\n---\nbody")},
		{
			name: "tools: without Workflow", agent: agent("without.md", "---\nname: a\ntools: Read, Bash(gh,jq)\n---\nbody"), rw: rw,
			wantErr: "workflow: agent code lists tools: without Workflow, so it cannot start /wfplug:probe; add Workflow to its tools:",
		},
		{
			name: "empty tools:", agent: agent("empty.md", "---\nname: a\ntools: []\n---\nbody"), rw: rw,
			wantErr: "lists tools: without Workflow",
		},
		{
			name: "disallowedTools: names Workflow", agent: agent("disallowed.md", "---\nname: a\ndisallowedTools: Workflow\n---\nbody"), rw: rw,
			wantErr: "workflow: agent code lists Workflow in disallowedTools:, so it cannot start /wfplug:probe; remove Workflow from its disallowedTools:",
		},
		{
			name: "disallowedTools: wins over tools:", agent: agent("both.md", "---\nname: a\ntools: Read, Workflow\ndisallowedTools: [Workflow]\n---\nbody"), rw: rw,
			wantErr: "lists Workflow in disallowedTools:",
		},
		{
			name: "broken tools: entry", agent: agent("broken.md", "---\nname: a\ntools: [Read, 42]\n---\nbody"), rw: rw,
			wantErr: "workflow: cannot read the tools of agent code (",
		},
		{
			name: "missing agent file", agent: filepath.Join(dir, "missing.md"), rw: rw,
			wantErr: "; fix the agent file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkWorkflowAgentTools(&harness.Harness{Agent: tt.agent}, "code", tt.rw)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestIterationPrompt(t *testing.T) {
	rw := &resolve.ResolvedWorkflow{Definition: "sample-pipeline", Name: "probe", Args: "--issue 7", PluginName: "wfplug"}

	t.Run("workflow command on every iteration, feedback noted once", func(t *testing.T) {
		var noted bool
		prompt, sanitized, inject, note := iterationPrompt(rw, 1, true, "", &noted)
		assert.Equal(t, "/wfplug:probe --issue 7", prompt)
		assert.Zero(t, sanitized)
		assert.False(t, inject)
		assert.False(t, note)

		prompt, _, inject, note = iterationPrompt(rw, 2, true, "FAIL: missing field", &noted)
		assert.Equal(t, "/wfplug:probe --issue 7", prompt, "a retry starts the same command, without the feedback")
		assert.False(t, inject)
		assert.True(t, note, "the first retry with feedback prints the note")

		prompt, _, _, note = iterationPrompt(rw, 3, true, "FAIL: still missing", &noted)
		assert.Equal(t, "/wfplug:probe --issue 7", prompt)
		assert.False(t, note, "the note prints once per run")
	})

	t.Run("workflow retry without feedback_mode append prints no note", func(t *testing.T) {
		var noted bool
		prompt, _, inject, note := iterationPrompt(rw, 2, false, "FAIL", &noted)
		assert.Equal(t, "/wfplug:probe --issue 7", prompt)
		assert.False(t, inject)
		assert.False(t, note)
	})

	t.Run("without a workflow the feedback prompt is unchanged", func(t *testing.T) {
		var noted bool
		prompt, _, inject, note := iterationPrompt(nil, 1, true, "", &noted)
		assert.Empty(t, prompt, "empty: the runtime uses its default prompt")
		assert.False(t, inject)
		assert.False(t, note)

		prompt, _, inject, _ = iterationPrompt(nil, 2, true, "FAIL: missing field", &noted)
		want, _ := buildFeedbackPrompt("FAIL: missing field")
		assert.Equal(t, want, prompt)
		assert.True(t, inject)
		assert.False(t, noted)
	})
}

func TestWorkflowLaunchInMetricsJSON(t *testing.T) {
	hash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	tests := []struct {
		name string
		rw   *resolve.ResolvedWorkflow
		want string
	}{
		{
			name: "remote",
			rw: &resolve.ResolvedWorkflow{
				Definition: "sample-pipeline", Name: "probe", Args: "--issue 7", PluginName: "wfplug",
				Source: "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA, Commit: cliWorkflowSHA, TreeHash: hash,
			},
			want: `{"definition":"sample-pipeline","source":"https://github.com/example-org/sample-pipeline/tree/` + cliWorkflowSHA +
				`","pin_sha256":"` + hash + `","command":"/wfplug:probe --issue 7"}`,
		},
		{
			name: "local",
			rw:   &resolve.ResolvedWorkflow{Definition: "self", Name: "probe", PluginName: "wfplug", Local: true, Source: "wfdef", TreeHash: hash},
			want: `{"definition":"self","source":"wfdef","pin_sha256":"` + hash + `","command":"/wfplug:probe"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, writeMetricsJSON(dir, aggregateMetrics{Runtime: "dummy", Workflow: newWorkflowLaunch(tt.rw)}))
			data, err := os.ReadFile(filepath.Join(dir, metricsFile))
			require.NoError(t, err)
			var got struct {
				Runtime  string          `json:"runtime"`
				Workflow json.RawMessage `json:"workflow"`
			}
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, "dummy", got.Runtime)
			assert.JSONEq(t, tt.want, string(got.Workflow))
		})
	}

	t.Run("absent without a workflow", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, writeMetricsJSON(dir, aggregateMetrics{Workflow: newWorkflowLaunch(nil)}))
		data, err := os.ReadFile(filepath.Join(dir, metricsFile))
		require.NoError(t, err)
		assert.NotContains(t, string(data), `"workflow"`)
	})
}

func TestRunAgent_WorkflowRefusedUnderPi(t *testing.T) {
	usePreScriptStub(t)
	dir := newWorkflowRunDir(t, "runtime: pi\n")

	var buf bytes.Buffer
	err := runAgent(context.Background(), "code", dir, "", t.TempDir(), "", nil, false, "", "", "", resolveFlags{maxDepth: 10, maxResources: 50},
		statusOpts{}, ui.New(&buf), false, runOverrideFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workflow: needs the claude runtime; agent "code" resolves to "pi"`)
}

func TestRunAgent_WorkflowResolutionError(t *testing.T) {
	usePreScriptStub(t)
	dir := newWorkflowRunDir(t, "")
	require.NoError(t, os.Remove(filepath.Join(dir, "wfdef", "workflows", "probe.js")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wfdef", "workflows", "other.js"), []byte("x"), 0o644))
	gitIn(t, dir, "add", "-A")

	var buf bytes.Buffer
	err := runAgent(context.Background(), "code", dir, "", t.TempDir(), "", nil, false, "", "", "", resolveFlags{maxDepth: 10, maxResources: 50},
		statusOpts{}, ui.New(&buf), false, runOverrideFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workflows "sample-pipeline": has no workflows/probe.js; the definition ships: other`)
}

// gitInit makes dir a git checkout with everything in it staged: a local
// workflows: source reads the index.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", "-A")
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

const cliWorkflowSHA2 = "89abcdef0123456789abcdef0123456789abcdef"

// workflowLockDir writes a code harness (harnessYAML) and a config.yaml
// with the given workflows: block, allowlisting example-org.
func workflowLockDir(t *testing.T, harnessYAML, workflows string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "harness"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "code.yaml"), []byte(harnessYAML), 0o644))
	writeWorkflowConfig(t, dir, workflows)
	return dir
}

func writeWorkflowConfig(t *testing.T, dir, workflows string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("allowed_remote_resources:\n  - https://github.com/example-org/\n"+workflows), 0o644))
}

const workflowHarnessYAML = "agent: agents/code.md\nrole: test\nworkflow:\n  definition: sample-pipeline\n  name: probe\n"

// refTrees serves a different definition tree per commit.
func refTrees() (map[string]map[string][]byte, gitfetch.TreeFetchFunc) {
	trees := map[string]map[string][]byte{
		cliWorkflowSHA:  cliWorkflowTree(),
		cliWorkflowSHA2: {".claude-plugin/plugin.json": []byte(`{"name":"wfplug"}`), "workflows/probe.js": []byte("export const meta = { name: 'probe', description: 'v2' };\n")},
	}
	return trees, func(_ context.Context, _, _, ref, _ string) (map[string][]byte, error) {
		return trees[ref], nil
	}
}

func pinnedWorkflowEntry(repo, sha string, files map[string][]byte) string {
	return "workflows:\n  - name: sample-pipeline\n    source: https://github.com/example-org/" + repo + "/tree/" + sha + "#sha256=" + fetch.ComputeTreeHash(files) + "\n"
}

func lockedWorkflowURL(t *testing.T, dir string) (string, int) {
	t.Helper()
	lf, err := lock.Load(filepath.Join(dir, "lock.yaml"))
	require.NoError(t, err)
	entry := lf.Lookup("code")
	require.NotNil(t, entry)
	if dep := lockedWorkflowDep(entry); dep != nil {
		return dep.URL, len(entry.Dependencies)
	}
	return "", len(entry.Dependencies)
}

func TestRunLock_WorkflowPinFreshness(t *testing.T) {
	trees, fetcher := refTrees()
	flags := resolveFlags{treeFetcher: fetcher}

	t.Run("unchanged pin stays up to date", func(t *testing.T) {
		dir := workflowLockDir(t, workflowHarnessYAML, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA]))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		var buf bytes.Buffer
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(&buf)))
		assert.Contains(t, buf.String(), "Lock entry for code is up to date")
	})
	t.Run("pin change without --update", func(t *testing.T) {
		dir := workflowLockDir(t, workflowHarnessYAML, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA]))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		writeWorkflowConfig(t, dir, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA2, trees[cliWorkflowSHA2]))
		var buf bytes.Buffer
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(&buf)))
		assert.Contains(t, buf.String(), "Lock entry for code is out of date: config.yaml pins workflows \"sample-pipeline\" to https://github.com/example-org/sample-pipeline/tree/"+cliWorkflowSHA2)
		url, _ := lockedWorkflowURL(t, dir)
		assert.Equal(t, "https://github.com/example-org/sample-pipeline/tree/"+cliWorkflowSHA2, url)
	})
	t.Run("definition replaced by another repository", func(t *testing.T) {
		dir := workflowLockDir(t, workflowHarnessYAML, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA]))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		writeWorkflowConfig(t, dir, pinnedWorkflowEntry("other-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA]))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		url, _ := lockedWorkflowURL(t, dir)
		assert.Equal(t, "https://github.com/example-org/other-pipeline/tree/"+cliWorkflowSHA, url)
	})
	t.Run("definition replaced by a local source", func(t *testing.T) {
		dir := workflowLockDir(t, workflowHarnessYAML, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA]))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		for p, c := range cliWorkflowTree() {
			full := filepath.Join(dir, "wfdef", filepath.FromSlash(p))
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, c, 0o644))
		}
		writeWorkflowConfig(t, dir, "workflows:\n  - name: sample-pipeline\n    source: wfdef\n")
		gitInit(t, dir)
		var buf bytes.Buffer
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(&buf)))
		assert.Contains(t, buf.String(), `workflows "sample-pipeline" is a local source in config.yaml, but lock.yaml pins`)
		url, n := lockedWorkflowURL(t, dir)
		assert.Empty(t, url, "the stale pin is cleared")
		assert.Zero(t, n)
		// The rewritten entry is now current.
		buf.Reset()
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(&buf)))
		assert.Contains(t, buf.String(), "is up to date")
	})
	t.Run("definition removed from config.yaml", func(t *testing.T) {
		dir := workflowLockDir(t, workflowHarnessYAML, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA]))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		writeWorkflowConfig(t, dir, "")
		var buf bytes.Buffer
		err := runLock(context.Background(), "code", dir, "", false, flags, ui.New(&buf))
		require.Error(t, err, "the lock is not left claiming to be current")
		assert.Contains(t, buf.String(), `workflow.definition "sample-pipeline" is not a config.yaml workflows: name any more`)
	})
	t.Run("workflow: removed from the harness", func(t *testing.T) {
		dir := workflowLockDir(t, workflowHarnessYAML, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA]))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "code.yaml"), []byte("agent: agents/code.md\nrole: test\n"), 0o644))
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard)))
		url, n := lockedWorkflowURL(t, dir)
		assert.Empty(t, url)
		assert.Zero(t, n)
	})
}

func TestLockWorkflowDrift(t *testing.T) {
	trees, _ := refTrees()
	pin := pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, trees[cliWorkflowSHA])
	locked := &lock.HarnessLock{Dependencies: []lock.DependencyEntry{{
		Field: resolve.WorkflowDependencyField, URL: "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA, SHA256: fetch.ComputeTreeHash(trees[cliWorkflowSHA]),
	}}}
	load := func(t *testing.T, dir string) config.ConfigReader {
		cfg, err := loadLockConfig(filepath.Join(dir, "config.yaml"))
		require.NoError(t, err)
		return cfg
	}
	composed := func(spec *harness.WorkflowSpec, calls *int) func() (*harness.Harness, error) {
		return func() (*harness.Harness, error) {
			*calls++
			return &harness.Harness{Workflow: spec}, nil
		}
	}
	noCompose := func() (*harness.Harness, error) {
		t.Fatal("a harness without base: is not composed")
		return nil, nil
	}
	harnessPath := func(dir string) string { return filepath.Join(dir, "harness", "code.yaml") }
	const baseChild = "base: base.yaml\nagent: agents/code.md\nrole: test\n"

	t.Run("no workflow anywhere", func(t *testing.T) {
		dir := workflowLockDir(t, "agent: agents/code.md\nrole: test\n", "")
		drift, recheck := lockWorkflowDrift(harnessPath(dir), nil, &lock.HarnessLock{}, noCompose)
		assert.Empty(t, drift)
		assert.False(t, recheck)
	})
	t.Run("no workflow and no base, but the lock pins one", func(t *testing.T) {
		dir := workflowLockDir(t, "agent: agents/code.md\nrole: test\n", pin)
		drift, _ := lockWorkflowDrift(harnessPath(dir), load(t, dir), locked, noCompose)
		assert.Contains(t, drift, "but the harness names no workflow: definition")
	})
	t.Run("pin missing from the lock", func(t *testing.T) {
		dir := workflowLockDir(t, workflowHarnessYAML, pin)
		drift, _ := lockWorkflowDrift(harnessPath(dir), load(t, dir), &lock.HarnessLock{}, noCompose)
		assert.Contains(t, drift, "which lock.yaml does not record")
	})
	t.Run("own workflow: wins over base: without composing", func(t *testing.T) {
		dir := workflowLockDir(t, "base: base.yaml\n"+workflowHarnessYAML, pin)
		drift, recheck := lockWorkflowDrift(harnessPath(dir), load(t, dir), locked, noCompose)
		assert.Empty(t, drift)
		assert.False(t, recheck)
	})
	t.Run("workflow: inherited through base: matches", func(t *testing.T) {
		dir := workflowLockDir(t, baseChild, pin)
		calls := 0
		drift, recheck := lockWorkflowDrift(harnessPath(dir), load(t, dir), locked, composed(&harness.WorkflowSpec{Definition: "sample-pipeline", Name: "probe"}, &calls))
		assert.Empty(t, drift)
		assert.False(t, recheck)
		assert.Equal(t, 1, calls)
	})
	t.Run("workflow: inherited through base: names a changed pin", func(t *testing.T) {
		dir := workflowLockDir(t, baseChild, pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA2, trees[cliWorkflowSHA2]))
		calls := 0
		drift, _ := lockWorkflowDrift(harnessPath(dir), load(t, dir), locked, composed(&harness.WorkflowSpec{Definition: "sample-pipeline", Name: "probe"}, &calls))
		assert.Contains(t, drift, "but lock.yaml has")
	})
	t.Run("base: chain without workflow:, but the lock pins one", func(t *testing.T) {
		dir := workflowLockDir(t, baseChild, pin)
		calls := 0
		drift, _ := lockWorkflowDrift(harnessPath(dir), load(t, dir), locked, composed(nil, &calls))
		assert.Contains(t, drift, "but the harness names no workflow: definition")
	})
	t.Run("base: chain that cannot be composed is rechecked", func(t *testing.T) {
		dir := workflowLockDir(t, baseChild, pin)
		drift, recheck := lockWorkflowDrift(harnessPath(dir), load(t, dir), locked, func() (*harness.Harness, error) {
			return nil, errors.New("not in the cache")
		})
		assert.Empty(t, drift)
		assert.True(t, recheck)
	})
	t.Run("unreadable harness defers to resolution", func(t *testing.T) {
		drift, recheck := lockWorkflowDrift(filepath.Join(t.TempDir(), "missing.yaml"), nil, locked, noCompose)
		assert.Empty(t, drift)
		assert.False(t, recheck)
	})
}

// TestRunLock_WorkflowFreshnessThroughBase checks the lock fast path
// against the workflow: a harness inherits from a local base: harness,
// and that the command the run-time drift warning suggests clears it.
func TestRunLock_WorkflowFreshnessThroughBase(t *testing.T) {
	trees, fetcher := refTrees()
	flags := resolveFlags{treeFetcher: fetcher}
	const child = "base: base.yaml\nagent: agents/code.md\nrole: test\n"
	const baseHarness = "agent: agents/code.md\nrole: test\nworkflow:\n  definition: sample-pipeline\n  name: probe\n"
	oldURL := "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA
	newURL := "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA2
	entryFor := func(name, sha string) string {
		return "  - name: " + name + "\n    source: https://github.com/example-org/sample-pipeline/tree/" + sha + "#sha256=" + fetch.ComputeTreeHash(trees[sha]) + "\n"
	}
	setup := func(t *testing.T, workflows string) string {
		t.Helper()
		dir := workflowLockDir(t, child, workflows)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "base.yaml"), []byte(baseHarness), 0o644))
		return dir
	}
	relock := func(t *testing.T, dir string) string {
		t.Helper()
		var buf bytes.Buffer
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(&buf)))
		return buf.String()
	}
	// warning resolves the definition as a run does and returns the
	// run-time drift warning.
	warning := func(t *testing.T, dir string) string {
		t.Helper()
		h, _, err := harness.LoadWithBase(context.Background(), filepath.Join(dir, "harness", "code.yaml"), harness.ComposeOpts{WorkspaceRoot: dir})
		require.NoError(t, err)
		cfg, err := loadLockConfig(filepath.Join(dir, "config.yaml"))
		require.NoError(t, err)
		rw, dep, err := resolveHarnessWorkflow(context.Background(), h, cfg, dir, resolve.ResolveOpts{
			WorkspaceRoot: dir, OrgAllowlist: cfg.AllowedResources(), TreeFetcher: fetcher,
		})
		require.NoError(t, err)
		var buf bytes.Buffer
		warnWorkflowLockDrift(ui.New(&buf), dir, dir, "code", rw, dep)
		return buf.String()
	}

	t.Run("unchanged pin stays up to date", func(t *testing.T) {
		dir := setup(t, "workflows:\n"+entryFor("sample-pipeline", cliWorkflowSHA))
		relock(t, dir)
		assert.Contains(t, relock(t, dir), "Lock entry for code is up to date")
		assert.Empty(t, warning(t, dir))
	})
	t.Run("pin of the inherited definition changes", func(t *testing.T) {
		dir := setup(t, "workflows:\n"+entryFor("sample-pipeline", cliWorkflowSHA))
		relock(t, dir)
		writeWorkflowConfig(t, dir, "workflows:\n"+entryFor("sample-pipeline", cliWorkflowSHA2))
		assert.Contains(t, warning(t, dir), "Run 'fullsend lock code")
		assert.Contains(t, relock(t, dir), "Lock entry for code is out of date")
		url, _ := lockedWorkflowURL(t, dir)
		assert.Equal(t, newURL, url)
		assert.Empty(t, warning(t, dir), "the suggested command cleared the warning")
	})
	t.Run("two definitions share the old pin and only the selected one changes", func(t *testing.T) {
		dir := setup(t, "workflows:\n"+entryFor("other", cliWorkflowSHA)+entryFor("sample-pipeline", cliWorkflowSHA))
		relock(t, dir)
		writeWorkflowConfig(t, dir, "workflows:\n"+entryFor("other", cliWorkflowSHA)+entryFor("sample-pipeline", cliWorkflowSHA2))
		assert.NotEmpty(t, warning(t, dir))
		assert.Contains(t, relock(t, dir), "Lock entry for code is out of date")
		url, _ := lockedWorkflowURL(t, dir)
		assert.Equal(t, newURL, url)
		assert.Empty(t, warning(t, dir))
	})
	t.Run("local definition switched to a remote one", func(t *testing.T) {
		dir := setup(t, "workflows:\n  - name: sample-pipeline\n    source: wfdef\n")
		for p, c := range cliWorkflowTree() {
			full := filepath.Join(dir, "wfdef", filepath.FromSlash(p))
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, c, 0o644))
		}
		gitInit(t, dir)
		var buf bytes.Buffer
		require.NoError(t, runLock(context.Background(), "code", dir, "", false, flags, ui.New(&buf)))
		// A local definition has nothing to lock; record an entry for the
		// agent so the fast path has something to compare.
		lf := &lock.LockFile{}
		data, err := os.ReadFile(filepath.Join(dir, "harness", "code.yaml"))
		require.NoError(t, err)
		lf.SetHarness("code", lock.HarnessLock{Source: "harness/code.yaml", SHA256: fetch.ComputeSHA256(data)})
		require.NoError(t, lock.Save(filepath.Join(dir, "lock.yaml"), lf))
		assert.Contains(t, relock(t, dir), "Lock entry for code is up to date")

		writeWorkflowConfig(t, dir, "workflows:\n"+entryFor("sample-pipeline", cliWorkflowSHA))
		assert.Contains(t, warning(t, dir), "Run 'fullsend lock code")
		assert.Contains(t, relock(t, dir), "which lock.yaml does not record")
		url, _ := lockedWorkflowURL(t, dir)
		assert.Equal(t, oldURL, url)
		assert.Empty(t, warning(t, dir))
	})
}

func TestWarnWorkflowLockDrift(t *testing.T) {
	trees, _ := refTrees()
	hash := fetch.ComputeTreeHash(trees[cliWorkflowSHA])
	url := "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA
	rw := &resolve.ResolvedWorkflow{Definition: "sample-pipeline"}
	writeLock := func(t *testing.T, dir string, entry *lock.HarnessLock) {
		lf := &lock.LockFile{}
		if entry != nil {
			lf.SetHarness("code", *entry)
		} else {
			lf.SetHarness("other", lock.HarnessLock{})
		}
		require.NoError(t, lock.Save(filepath.Join(dir, "lock.yaml"), lf))
	}
	matching := &lock.HarnessLock{Dependencies: []lock.DependencyEntry{{Field: resolve.WorkflowDependencyField, URL: url, SHA256: hash}}}
	tests := []struct {
		name  string
		entry *lock.HarnessLock
		dep   *resolve.Dependency
		want  string
	}{
		{name: "match", entry: matching, dep: &resolve.Dependency{URL: url, SHA256: hash}},
		{name: "pin changed", entry: matching, dep: &resolve.Dependency{URL: url, SHA256: "ff" + hash[2:]}, want: "Workflow definition differs from lock.yaml: config.yaml pins workflows \"sample-pipeline\""},
		{name: "local now", entry: matching, want: "is a local source in config.yaml"},
		{name: "local, nothing locked", entry: &lock.HarnessLock{}},
		{name: "agent not locked", dep: &resolve.Dependency{URL: url, SHA256: hash}, want: "lock.yaml has no entry for code"},
		{name: "agent not locked, local source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLock(t, dir, tt.entry)
			var buf bytes.Buffer
			warnWorkflowLockDrift(ui.New(&buf), dir, dir, "code", rw, tt.dep)
			if tt.want == "" {
				assert.Empty(t, buf.String())
				return
			}
			assert.Contains(t, buf.String(), tt.want)
			assert.Contains(t, buf.String(), "Run 'fullsend lock code")
		})
	}
	t.Run("no lock file", func(t *testing.T) {
		var buf bytes.Buffer
		warnWorkflowLockDrift(ui.New(&buf), t.TempDir(), ".", "code", rw, &resolve.Dependency{URL: url, SHA256: hash})
		assert.Empty(t, buf.String())
	})
}

// TestRunLock_WorkflowSharesURLWithPluginOrSkill checks that a workflow
// definition and a plugins: or skills: entry backed by the same tree URL
// each keep their lock binding through lock and replay. The tree has no
// .claude-plugin/plugin.json, so each copy's Claude Code plugin name is
// its directory name ("sample-pipeline" and "shared") and they do not
// collide.
func TestRunLock_WorkflowSharesURLWithPluginOrSkill(t *testing.T) {
	files := map[string][]byte{
		"plugin.json":        []byte(`{}`),
		"workflows/probe.js": []byte("export const meta = { name: 'probe' };\n"),
		"SKILL.md":           []byte("---\nname: shared\ndescription: shared tree\n---\n# shared\n"),
	}
	treeURL := "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA + "/pipelines/shared"
	pinned := treeURL + "#sha256=" + fetch.ComputeTreeHash(files)
	fetcher := func(context.Context, string, string, string, string) (map[string][]byte, error) { return files, nil }
	workflows := "workflows:\n  - name: sample-pipeline\n    source: " + pinned + "\n"

	for _, tc := range []struct {
		name, harnessExtra, field string
		check                     func(t *testing.T, h *harness.Harness)
	}{
		{
			name:         "plugin",
			harnessExtra: "plugins:\n  - " + pinned + "\n",
			field:        "plugins[0]",
			check: func(t *testing.T, h *harness.Harness) {
				require.Len(t, h.Plugins, 1)
				assert.Equal(t, "shared", filepath.Base(h.Plugins[0].Path), "the plugin is bound to its cached tree")
			},
		},
		{
			name:         "skill",
			harnessExtra: "skills:\n  - " + pinned + "\n",
			field:        "skills[0]",
			check: func(t *testing.T, h *harness.Harness) {
				require.NotEmpty(t, h.Skills)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := workflowLockDir(t, workflowHarnessYAML+tc.harnessExtra, workflows)
			require.NoError(t, runLock(context.Background(), "code", dir, "", false, resolveFlags{treeFetcher: gitfetch.TreeFetchFunc(fetcher)}, ui.New(io.Discard)))
			lf, err := lock.Load(filepath.Join(dir, "lock.yaml"))
			require.NoError(t, err)
			entry := lf.Lookup("code")
			require.NotNil(t, entry)
			fields := map[string]string{}
			for _, d := range entry.Dependencies {
				fields[d.Field] = d.URL
			}
			assert.Equal(t, map[string]string{resolve.WorkflowDependencyField: treeURL, tc.field: treeURL}, fields)

			h, err := harness.Load(filepath.Join(dir, "harness", "code.yaml"))
			require.NoError(t, err)
			require.NoError(t, h.ResolveRelativeTo(dir))
			result, err := resolveFromLock(h, entry, dir, []string{"https://github.com/example-org/"}, ui.New(io.Discard))
			require.NoError(t, err)
			assert.Len(t, result.Deps, 2)
			tc.check(t, h)
		})
	}
}

// TestRunLock_SharedSkillAcrossForgeVariants checks that a skill URL the
// forge variants hold at different skills[N] indices is locked once (by
// URL; only the workflow field is keyed by field and URL), and that each
// variant's lock replay gets its own skills.
func TestRunLock_SharedSkillAcrossForgeVariants(t *testing.T) {
	shared := map[string][]byte{"SKILL.md": []byte("---\nname: shared\ndescription: shared skill\n---\n# shared\n")}
	pinned := "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA + "/skills/shared#sha256=" + fetch.ComputeTreeHash(shared)
	sharedURL, _, _ := harness.ParseIntegrityHash(pinned)
	fetcher := gitfetch.TreeFetchFunc(func(context.Context, string, string, string, string) (map[string][]byte, error) {
		return shared, nil
	})
	// github holds the shared skill at skills[1], behind a local skill;
	// gitlab holds it at skills[0].
	harnessYAML := "agent: agents/code.md\nrole: test\nforge:\n  github:\n    skills:\n      - skills/local\n      - " + pinned +
		"\n  gitlab:\n    skills:\n      - " + pinned + "\n"
	dir := workflowLockDir(t, harnessYAML, "")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "skills", "local"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "skills", "local", "SKILL.md"), []byte("---\nname: local\ndescription: local skill\n---\n# local\n"), 0o644))
	require.NoError(t, runLock(context.Background(), "code", dir, "", false, resolveFlags{treeFetcher: fetcher}, ui.New(io.Discard)))

	lf, err := lock.Load(filepath.Join(dir, "lock.yaml"))
	require.NoError(t, err)
	entry := lf.Lookup("code")
	require.NotNil(t, entry)
	var fields []string
	for _, d := range entry.Dependencies {
		if d.URL == sharedURL {
			fields = append(fields, d.Field)
		}
	}
	assert.Equal(t, []string{"skills[1]"}, fields, "the shared skill is locked once")

	for platform, want := range map[string][]string{"github": {"local", "shared"}, "gitlab": {"shared"}} {
		t.Run(platform, func(t *testing.T) {
			policy := fetch.DefaultPolicy
			policy.Offline = true
			h, _, err := harness.LoadWithBase(context.Background(), filepath.Join(dir, "harness", "code.yaml"), harness.ComposeOpts{
				WorkspaceRoot: dir,
				FetchPolicy:   policy,
				ForgePlatform: platform,
				TreeFetcher:   fetcher,
			})
			require.NoError(t, err)
			require.NoError(t, h.ResolveRelativeTo(dir))
			_, err = resolveFromLock(h, entry, dir, []string{"https://github.com/example-org/"}, ui.New(io.Discard))
			require.NoError(t, err)
			var got []string
			for _, s := range h.Skills {
				got = append(got, filepath.Base(s.Source))
			}
			assert.Equal(t, want, got)
		})
	}
}

// TestRunLock_WorkflowNamespaceCollisionWithURLPlugin checks that lock
// refuses a URL plugin whose fetched manifest name is the definition's
// plugin name, with the error fullsend run gives for the same harness.
func TestRunLock_WorkflowNamespaceCollisionWithURLPlugin(t *testing.T) {
	usePreScriptStub(t)
	files := cliWorkflowTree()
	treeURL := "https://github.com/example-org/sample-pipeline/tree/" + cliWorkflowSHA + "/plugins/helpers"
	pinned := treeURL + "#sha256=" + fetch.ComputeTreeHash(files)
	flags := resolveFlags{maxDepth: 10, maxResources: 50, treeFetcher: gitfetch.TreeFetchFunc(func(context.Context, string, string, string, string) (map[string][]byte, error) {
		return files, nil
	})}
	dir := newSkipHarnessDir(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "code.yaml"), []byte(workflowHarnessYAML+"plugins:\n  - "+pinned+"\n"), 0o644))
	writeWorkflowConfig(t, dir, "agents:\n  - harness/code.yaml\n"+pinnedWorkflowEntry("sample-pipeline", cliWorkflowSHA, files))
	want := `resolving workflow definition: workflows "sample-pipeline": plugins[0] "`
	wantNS := `has the Claude Code plugin name "wfplug", the same as the definition's plugin name "wfplug"`

	lockErr := runLock(context.Background(), "code", dir, "", false, flags, ui.New(io.Discard))
	require.Error(t, lockErr)
	assert.Contains(t, lockErr.Error(), want)
	assert.Contains(t, lockErr.Error(), wantNS)
	_, statErr := os.Stat(filepath.Join(dir, "lock.yaml"))
	assert.True(t, os.IsNotExist(statErr), "nothing is locked")

	runErr := runAgent(context.Background(), "code", dir, "", t.TempDir(), "", nil, false, "", "", "", flags,
		statusOpts{}, ui.New(io.Discard), false, runOverrideFlags{})
	require.Error(t, runErr)
	assert.Equal(t, runErr.Error(), lockErr.Error(), "lock refuses what run refuses, with the same error")
}

// TestRunAgent_InvalidWorkflowsEntryRejected checks that runAgent
// validates config.yaml workflows: on every run, before the sandbox is
// created, also for a harness without workflow:.
func TestRunAgent_InvalidWorkflowsEntryRejected(t *testing.T) {
	usePreScriptStub(t)
	dir := newSkipHarnessDir(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("agents:\n  - harness/code.yaml\nworkflows:\n  - name: bad name\n    source: wfdef\n"), 0o644))

	var buf bytes.Buffer
	err := runAgent(context.Background(), "code", dir, "", t.TempDir(), "", nil, false, "", "", "", resolveFlags{maxDepth: 10, maxResources: 50},
		statusOpts{}, ui.New(&buf), false, runOverrideFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workflows[0] "bad name": name may contain only letters, digits, _ and -`)
	assert.NotContains(t, err.Error(), "creating sandbox")
	assert.NotContains(t, buf.String(), "Creating sandbox")
}

func TestExpandWorkflowArgs(t *testing.T) {
	env := map[string]string{"ISSUE_NUMBER": "42", "EVIL": "1\n/other:cmd"}
	expand := func(k string) string { return env[k] }

	got, err := expandWorkflowArgs("issue ${ISSUE_NUMBER}", expand)
	require.NoError(t, err)
	assert.Equal(t, "issue 42", got)

	got, err = expandWorkflowArgs("issue ${UNSET}", expand)
	require.NoError(t, err)
	assert.Equal(t, "issue ", got, "an unset variable expands empty, as in env:")

	_, err = expandWorkflowArgs("issue ${EVIL}", expand)
	require.Error(t, err)
	assert.Equal(t, `workflow.args "issue ${EVIL}" expands to more than one line; the variables it names must not hold NUL, carriage return or newline characters`, err.Error())
}

// Args reach the model prompt, the plan, metrics.json and traces, so a
// credential-shaped or runner-only reference is refused before anything
// is expanded, and the expander is never asked for its value.
func TestExpandWorkflowArgs_CredentialReferenceRefused(t *testing.T) {
	expand := func(k string) string {
		t.Errorf("expander called for %s", k)
		return ""
	}
	for _, name := range []string{"GH_TOKEN", "OTEL_EXPORTER_OTLP_HEADERS", "MY_API_KEY", "DB_PASSWORD", "X_SECRET_Y", "ACTIONS_ID_TOKEN_REQUEST_URL", "FULLSEND_GCP_OIDC_URL", "GH_WORKFLOW_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			_, err := expandWorkflowArgs("issue ${ISSUE_NUMBER} ${"+name+"}", expand)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "workflow.args references ${"+name+"}, which names a credential (")
			assert.Contains(t, err.Error(), "); pass work-item identifiers such as ${ISSUE_NUMBER} instead")
		})
	}
	_, err := expandWorkflowArgs("issue ${FULLSEND_GCP_OIDC_URL}", expand)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "(a runner-only credential no harness may expand)")
}

// A value that looks like a credential in an allowed variable is refused
// after expansion, without printing it.
func TestExpandWorkflowArgs_CredentialValueRefused(t *testing.T) {
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	expand := func(k string) string {
		if k == "ISSUE_NUMBER" {
			return secret
		}
		return ""
	}
	_, err := expandWorkflowArgs("issue ${ISSUE_NUMBER}", expand)
	require.Error(t, err)
	assert.Equal(t, "workflow.args expands to a value that looks like a credential; pass work-item identifiers only", err.Error())
	assert.NotContains(t, err.Error(), secret)
}

// A credential reference fails the run at plan time, before the sandbox.
func TestRunAgent_WorkflowArgsCredentialRefused(t *testing.T) {
	usePreScriptStub(t)
	t.Setenv("ISSUE_NUMBER", "ghp_"+"abcdefghijklmnopqrstuvwxyz0123456789")
	dir := newWorkflowRunDir(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "harness", "code.yaml"),
		[]byte("agent: agents/code.md\nrole: test\nworkflow:\n  definition: sample-pipeline\n  name: probe\n  args: issue ${ISSUE_NUMBER}\n"), 0o644))

	var buf bytes.Buffer
	err := runAgent(context.Background(), "code", dir, "", t.TempDir(), "", nil, false, "", "", "", resolveFlags{maxDepth: 10, maxResources: 50},
		statusOpts{}, ui.New(&buf), false, runOverrideFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workflow.args expands to a value that looks like a credential")
	assert.NotContains(t, buf.String(), "Creating sandbox")
	assert.NotContains(t, buf.String()+err.Error(), "abcdefghijklmnopqrstuvwxyz0123456789")
}

func TestWorkflowSpanAttr(t *testing.T) {
	attr := workflowSpanAttr(&resolve.ResolvedWorkflow{PluginName: "wfplug", Name: "probe", Args: "issue 7"})
	assert.Equal(t, "fullsend.workflow", string(attr.Key))
	assert.Equal(t, "/wfplug:probe", attr.Value.AsString(), "args stay out of traces")
}
