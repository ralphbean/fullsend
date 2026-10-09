package harness

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestWorkflowSpec_ParseAndValidate(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    *WorkflowSpec
		wantErr string
	}{
		{name: "absent", yaml: ""},
		{
			name: "full",
			yaml: "workflow:\n  definition: sample-pipeline\n  name: probe\n  args: \"triage issue 7, then \\\"a b\\\"\"\n",
			want: &WorkflowSpec{Definition: "sample-pipeline", Name: "probe", Args: `triage issue 7, then "a b"`},
		},
		{
			name: "no args",
			yaml: "workflow:\n  definition: sample\n  name: run_all\n",
			want: &WorkflowSpec{Definition: "sample", Name: "run_all"},
		},
		{name: "missing definition", yaml: "workflow:\n  name: probe\n", wantErr: "workflow.definition is required"},
		{name: "bad definition", yaml: "workflow:\n  definition: a/b\n  name: probe\n", wantErr: `workflow.definition "a/b" may contain only`},
		{name: "missing name", yaml: "workflow:\n  definition: sample\n", wantErr: "workflow.name is required"},
		{name: "bad name", yaml: "workflow:\n  definition: sample\n  name: probe.js\n", wantErr: `workflow.name "probe.js" may contain only`},
		{name: "newline in args", yaml: "workflow:\n  definition: sample\n  name: probe\n  args: \"a\\nb\"\n", wantErr: "workflow.args must be a single line"},
		{name: "carriage return in args", yaml: "workflow:\n  definition: sample\n  name: probe\n  args: \"a\\rb\"\n", wantErr: "workflow.args must be a single line"},
		{
			name: "work-item variable in args",
			yaml: "workflow:\n  definition: sample\n  name: probe\n  args: issue ${ISSUE_NUMBER}\n",
			want: &WorkflowSpec{Definition: "sample", Name: "probe", Args: "issue ${ISSUE_NUMBER}"},
		},
		{
			name:    "credential variable in args",
			yaml:    "workflow:\n  definition: sample\n  name: probe\n  args: issue ${ISSUE_NUMBER} ${GH_TOKEN}\n",
			wantErr: "workflow.args references ${GH_TOKEN}, which names a credential (the *_TOKEN family (credential-shaped names)); pass work-item identifiers such as ${ISSUE_NUMBER} instead",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h Harness
			require.NoError(t, yaml.Unmarshal([]byte("agent: agents/a.md\nrole: test\n"+tt.yaml), &h))
			err := h.Validate()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, h.Workflow)
		})
	}
}

func TestLoadWithBase_Workflow(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		child string
		want  *WorkflowSpec
	}{
		{
			name:  "child inherits base workflow",
			base:  "workflow:\n  definition: sample\n  name: probe\n  args: x\n",
			child: "",
			want:  &WorkflowSpec{Definition: "sample", Name: "probe", Args: "x"},
		},
		{
			name:  "child replaces base workflow wholesale",
			base:  "workflow:\n  definition: sample\n  name: probe\n  args: x\n",
			child: "workflow:\n  definition: other\n  name: run\n",
			want:  &WorkflowSpec{Definition: "other", Name: "run"},
		},
		{
			name:  "child only",
			base:  "",
			child: "workflow:\n  definition: other\n  name: run\n",
			want:  &WorkflowSpec{Definition: "other", Name: "run"},
		},
		{name: "neither", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestHarness(t, dir, "base.yaml", "agent: agents/base.md\nrole: test\n"+tt.base)
			path := writeTestHarness(t, dir, "child.yaml", "base: base.yaml\n"+tt.child)
			h, _, err := LoadWithBase(context.Background(), path, ComposeOpts{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, h.Workflow)
		})
	}
}

func TestMergeBaseIntoChild_WorkflowIsCopied(t *testing.T) {
	base := &Harness{Workflow: &WorkflowSpec{Definition: "sample", Name: "probe"}}
	child := &Harness{}
	mergeBaseIntoChild(base, child)
	require.NotNil(t, child.Workflow)
	child.Workflow.Args = "changed"
	assert.Empty(t, base.Workflow.Args, "the child must not share the base's WorkflowSpec")
}

func TestWorkflowUnderForgeOrOverlaysIsIgnored(t *testing.T) {
	for name, block := range map[string]string{
		"forge":    "forge:\n  github:\n    workflow:\n      definition: sample\n      name: probe\n",
		"overlays": "overlays:\n  - when: \"true\"\n    workflow:\n      definition: sample\n      name: probe\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTestHarness(t, t.TempDir(), "code.yaml", "agent: agents/a.md\nrole: test\n"+block)
			h, _, err := LoadWithBase(context.Background(), path, ComposeOpts{ForgePlatform: "github"})
			require.NoError(t, err)
			assert.Nil(t, h.Workflow, "workflow: is top-level only; under forge: or overlays: it is ignored, not an error")
		})
	}
}

func TestCredentialShapedEnvName(t *testing.T) {
	for _, name := range []string{
		"GH_TOKEN", "GITHUB_TOKEN", "OTEL_EXPORTER_OTLP_HEADERS", "otel_service_name", "MY_API_KEY", "SIGNING_PRIVATE_KEY", "AWS_ACCESS_KEY", "APP_SECRET_KEY",
		"DB_PASSWORD", "GOOGLE_APPLICATION_CREDENTIALS", "X_SECRET_Y", "WEBHOOK_SECRET", "HTTPS_PROXY", "https_proxy",
		"TOKEN", "PASSWORD", "SECRET",
	} {
		rule, ok := CredentialShapedEnvName(name)
		assert.True(t, ok, name)
		assert.NotEmpty(t, rule, name)
	}
	for _, name := range []string{"ISSUE_NUMBER", "ISSUE_KEY", "PR_NUMBER", "REPO_FULL_NAME", "KEYWORDS", "TOKENIZER", "FULLSEND_DIR"} {
		_, ok := CredentialShapedEnvName(name)
		assert.False(t, ok, name)
	}
}

func TestCheckWorkflowArgsVariables(t *testing.T) {
	for _, name := range []string{"GH_TOKEN", "OTEL_EXPORTER_OTLP_HEADERS", "MY_API_KEY", "DB_PASSWORD", "X_SECRET_Y"} {
		err := CheckWorkflowArgsVariables("issue ${"+name+"}", nil)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "workflow.args references ${"+name+"}, which names a credential (")
	}
	require.NoError(t, CheckWorkflowArgsVariables("issue ${ISSUE_NUMBER}", nil))
	require.NoError(t, CheckWorkflowArgsVariables("literal $GH_TOKEN text", nil), "args without ${ are not expanded")

	extra := func(name string) (string, bool) { return "runner-only", name == "RUNNER_ONLY" }
	err := CheckWorkflowArgsVariables("${ISSUE_NUMBER} ${RUNNER_ONLY}", extra)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "${RUNNER_ONLY}, which names a credential (runner-only)")
	require.NoError(t, CheckWorkflowArgsVariables("${ISSUE_NUMBER}", extra))
}
