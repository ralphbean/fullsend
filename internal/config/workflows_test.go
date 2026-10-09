package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	testWorkflowSHA  = "0123456789abcdef0123456789abcdef01234567"
	testWorkflowHash = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
)

func testWorkflowURL(suffix string) string {
	return "https://github.com/example-org/sample-pipeline/tree/" + testWorkflowSHA + suffix + "#sha256=" + testWorkflowHash
}

func TestValidateWorkflowEntries(t *testing.T) {
	allow := []string{"https://github.com/example-org/"}
	tests := []struct {
		name    string
		entries []WorkflowEntry
		wantErr string
	}{
		{name: "empty list", entries: nil},
		{name: "remote root", entries: []WorkflowEntry{{Name: "sample-pipeline", Source: testWorkflowURL("")}}},
		{name: "remote subpath", entries: []WorkflowEntry{{Name: "sample", Source: testWorkflowURL("/pipelines/sample")}}},
		{name: "local root", entries: []WorkflowEntry{{Name: "self", Source: "."}}},
		{name: "local subdir", entries: []WorkflowEntry{{Name: "self", Source: "pipelines/sample"}}},
		{name: "missing name", entries: []WorkflowEntry{{Source: "."}}, wantErr: "workflows[0]: name is required"},
		{name: "bad name", entries: []WorkflowEntry{{Name: "a b", Source: "."}}, wantErr: `workflows[0] "a b": name may contain only letters`},
		{
			name:    "duplicate name ignores case",
			entries: []WorkflowEntry{{Name: "Sample", Source: "."}, {Name: "sample", Source: "x"}},
			wantErr: `workflows[1] "sample": name is already used by workflows[0]`,
		},
		{name: "reserved name tree", entries: []WorkflowEntry{{Name: "tree", Source: "."}}, wantErr: `workflows[0] "tree": name is reserved, because the definition's cache directory already holds an entry called "tree"`},
		{name: "reserved name Tree", entries: []WorkflowEntry{{Name: "Tree", Source: "."}}, wantErr: `workflows[0] "Tree": name is reserved`},
		{name: "reserved name TREE", entries: []WorkflowEntry{{Name: "TREE", Source: "."}}, wantErr: `workflows[0] "TREE": name is reserved`},
		{name: "reserved name Content", entries: []WorkflowEntry{{Name: "Content", Source: "."}}, wantErr: `workflows[0] "Content": name is reserved, because the definition's cache directory already holds an entry called "content"`},
		{name: "name containing tree is fine", entries: []WorkflowEntry{{Name: "tree-walk", Source: "."}}},
		{name: "missing source", entries: []WorkflowEntry{{Name: "sample"}}, wantErr: `workflows[0] "sample": source is required`},
		{name: "newline in source", entries: []WorkflowEntry{{Name: "sample", Source: "a\nb"}}, wantErr: "must not contain NUL"},
		{
			name:    "branch ref",
			entries: []WorkflowEntry{{Name: "sample", Source: "https://github.com/example-org/sample-pipeline/tree/main#sha256=" + testWorkflowHash}},
			wantErr: `source ref "main" is not a commit sha; pin the commit sha`,
		},
		{
			name:    "short sha",
			entries: []WorkflowEntry{{Name: "sample", Source: "https://github.com/example-org/sample-pipeline/tree/0123abc#sha256=" + testWorkflowHash}},
			wantErr: "pin the commit sha",
		},
		{
			name:    "missing hash",
			entries: []WorkflowEntry{{Name: "sample", Source: "https://github.com/example-org/sample-pipeline/tree/" + testWorkflowSHA}},
			wantErr: "#sha256=<64 hex characters>",
		},
		{
			name:    "blob url",
			entries: []WorkflowEntry{{Name: "sample", Source: "https://github.com/example-org/sample-pipeline/blob/" + testWorkflowSHA + "/x#sha256=" + testWorkflowHash}},
			wantErr: "must use /tree/",
		},
		{
			name:    "gitlab url",
			entries: []WorkflowEntry{{Name: "sample", Source: "https://gitlab.com/example-org/sample-pipeline/-/tree/" + testWorkflowSHA + "#sha256=" + testWorkflowHash}},
			wantErr: `source is on "gitlab", but a remote workflow definition must be a github.com tree URL today, because tree fetching (shared with plugins: and skills:) supports only github.com; use a local path source, which works on any forge`,
		},
		{
			name:    "not a forge url",
			entries: []WorkflowEntry{{Name: "sample", Source: "https://example.com/sample#sha256=" + testWorkflowHash}},
			wantErr: "must be a GitHub tree URL",
		},
		{
			name:    "not allowlisted",
			entries: []WorkflowEntry{{Name: "sample", Source: "https://github.com/other-org/sample-pipeline/tree/" + testWorkflowSHA + "#sha256=" + testWorkflowHash}},
			wantErr: "is not covered by allowed_remote_resources; add a prefix such as https://github.com/other-org/sample-pipeline/",
		},
		{name: "dot segment", entries: []WorkflowEntry{{Name: "sample", Source: testWorkflowURL("/./x")}}, wantErr: `must not contain "." or ".." segments`},
		{name: "http", entries: []WorkflowEntry{{Name: "sample", Source: "http://github.com/x"}}, wantErr: "must use https"},
		{name: "other scheme", entries: []WorkflowEntry{{Name: "sample", Source: "git://github.com/x"}}, wantErr: "scheme is not supported"},
		{name: "absolute", entries: []WorkflowEntry{{Name: "sample", Source: "/etc"}}, wantErr: "is an absolute path"},
		{name: "backslash", entries: []WorkflowEntry{{Name: "sample", Source: `a\b`}}, wantErr: "contains a backslash"},
		{name: "traversal", entries: []WorkflowEntry{{Name: "sample", Source: "a/../../b"}}, wantErr: `contains ".."`},
		{name: "git dir", entries: []WorkflowEntry{{Name: "sample", Source: ".git"}}, wantErr: `source ".git" names .git/, which is never part of a definition`},
		{name: "git dir below", entries: []WorkflowEntry{{Name: "sample", Source: "sub/.git/hooks"}}, wantErr: `names .git/`},
		{name: "cache dir", entries: []WorkflowEntry{{Name: "sample", Source: ".fullsend/.fullsend-cache/resources"}}, wantErr: `names .fullsend-cache/`},
		{name: "dot-git prefix is a normal name", entries: []WorkflowEntry{{Name: "sample", Source: ".github/workflow-def"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkflowEntries(tt.entries, allow)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateWorkflowEntries_ManagedLayerSkipsAllowlist(t *testing.T) {
	entries := []WorkflowEntry{{Name: "sample", Source: testWorkflowURL("")}}
	assert.NoError(t, validateWorkflowEntries(entries, nil, false))
	assert.Error(t, validateWorkflowEntries(entries, nil, true))
}

func TestMergeWorkflowEntries(t *testing.T) {
	parent := []WorkflowEntry{{Name: "a", Source: "pa"}, {Name: "b", Source: "pb"}}
	tests := []struct {
		name  string
		child []WorkflowEntry
		want  []WorkflowEntry
	}{
		{name: "empty child keeps parent", child: []WorkflowEntry{}, want: parent},
		{
			name:  "same name replaces in place, new appended",
			child: []WorkflowEntry{{Name: "c", Source: "cc"}, {Name: "A", Source: "ca"}},
			want:  []WorkflowEntry{{Name: "A", Source: "ca"}, {Name: "b", Source: "pb"}, {Name: "c", Source: "cc"}},
		},
		{
			name:  "child duplicate survives for validation",
			child: []WorkflowEntry{{Name: "a", Source: "x"}, {Name: "a", Source: "y"}},
			want:  []WorkflowEntry{{Name: "a", Source: "x"}, {Name: "b", Source: "pb"}, {Name: "a", Source: "y"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mergeWorkflowEntries(parent, tt.child))
		})
	}
	assert.Equal(t, []WorkflowEntry{{Name: "x"}}, mergeWorkflowEntries(nil, []WorkflowEntry{{Name: "x"}}))
}

func TestWorkflowEntries_Layered(t *testing.T) {
	base := `workflows:
  - name: shared
    source: ` + testWorkflowURL("") + `
  - name: other
    source: pipelines/other
`
	t.Run("overlay omits key inherits base", func(t *testing.T) {
		w, err := ParsePerRepoConfigWriterLayered([]byte("version: \"1\"\n"), []byte(base))
		require.NoError(t, err)
		assert.Equal(t, []string{"shared", "other"}, WorkflowNames(w.WorkflowEntries()))
	})
	t.Run("overlay replaces by name", func(t *testing.T) {
		w, err := ParsePerRepoConfigWriterLayered([]byte("workflows:\n  - name: other\n    source: .\n"), []byte(base))
		require.NoError(t, err)
		got, ok := FindWorkflowEntry(w.WorkflowEntries(), "OTHER")
		require.True(t, ok)
		assert.Equal(t, ".", got.Source)
		assert.Len(t, w.WorkflowEntries(), 2)
		_, ok = FindWorkflowEntry(w.WorkflowEntries(), "missing")
		assert.False(t, ok)
	})
	t.Run("overlay empty list does not clear the base", func(t *testing.T) {
		// Like agents:, an explicit empty list merges by name with
		// nothing to merge, so the parent's entries stay.
		w, err := ParsePerRepoConfigWriterLayered([]byte("workflows: []\n"), []byte(base))
		require.NoError(t, err)
		assert.Equal(t, []string{"shared", "other"}, WorkflowNames(w.WorkflowEntries()))
	})
	t.Run("defaults have none", func(t *testing.T) {
		assert.Nil(t, (&perRepoDefaults{}).WorkflowEntries())
	})
}

func TestPerRepoConfig_WorkflowsRoundTripAndValidate(t *testing.T) {
	input := `version: "1"
allowed_remote_resources:
  - https://github.com/example-org/
workflows:
  - name: sample-pipeline
    source: ` + testWorkflowURL("") + `
`
	w, err := ParsePerRepoConfigWriter([]byte(input))
	require.NoError(t, err)
	require.NoError(t, w.Validate())
	out, err := w.Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(out), "workflows:")
	assert.Contains(t, string(out), "name: sample-pipeline")
	assert.Contains(t, string(out), testWorkflowSHA)

	bad, err := ParsePerRepoConfigWriter([]byte("workflows:\n  - name: x\n    source: /abs\n"))
	require.NoError(t, err)
	err = bad.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute path")
}

func TestManagedConfig_Workflows(t *testing.T) {
	assert.True(t, yamlKeysOf(perRepoConfig{})["workflows"], "workflows is a known managed-config key")

	var o ManagedConfig
	require.NoError(t, yaml.Unmarshal([]byte("workflows:\n  - name: sample\n    source: .\n"), &o))
	assert.Equal(t, []string{"sample"}, WorkflowNames(o.Writer().WorkflowEntries()))
	require.NoError(t, ValidateManagedLayer(o.Writer()))
	require.NoError(t, ValidateMergedManaged(o.Writer()))

	var unknown ManagedConfig
	err := yaml.Unmarshal([]byte("workflows:\n  - name: sample\n    source: .\n    ref: main\n"), &unknown)
	require.Error(t, err, "workflow entries decode strictly in managed config")
	assert.Contains(t, err.Error(), "ref")

	parent := asPerRepo(o.Writer())
	var child ManagedConfig
	require.NoError(t, yaml.Unmarshal([]byte("workflows:\n  - name: extra\n    source: x\n"), &child))
	merged := MergeManaged(parent, child.Writer())
	assert.Equal(t, []string{"sample", "extra"}, WorkflowNames(merged.WorkflowEntries()))
	// The clone must not share the parent's backing array.
	asPerRepo(merged).Workflows[0].Source = "changed"
	assert.Equal(t, ".", parent.Workflows[0].Source)

	var badLayer ManagedConfig
	require.NoError(t, yaml.Unmarshal([]byte("workflows:\n  - name: \"\"\n    source: .\n"), &badLayer))
	assert.Error(t, ValidateManagedLayer(badLayer.Writer()))
	assert.Error(t, ValidateMergedManaged(badLayer.Writer()))
}

func TestWorkflowEntry_IsRemote(t *testing.T) {
	assert.True(t, WorkflowEntry{Source: testWorkflowURL("")}.IsRemote())
	assert.False(t, WorkflowEntry{Source: "."}.IsRemote())
	assert.True(t, ValidWorkflowName("sample_1-x"))
	assert.False(t, ValidWorkflowName("a/b"))
}

func TestPerRepoConfigWriter_SetWorkflows(t *testing.T) {
	cfg, err := ParsePerRepoConfigWriter([]byte("runtime: dummy\n"))
	require.NoError(t, err)
	assert.Nil(t, cfg.WorkflowEntries())

	cfg.SetWorkflows([]WorkflowEntry{{Name: "sample-pipeline", Source: "pipelines/sample-pipeline"}})
	data, err := cfg.Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(data), "workflows:\n    - name: sample-pipeline\n      source: pipelines/sample-pipeline\n")

	reparsed, err := ParsePerRepoConfigWriter(data)
	require.NoError(t, err)
	assert.Equal(t, []string{"sample-pipeline"}, WorkflowNames(reparsed.WorkflowEntries()))

	// nil drops the key again, so a restored config reads as before.
	reparsed.SetWorkflows(nil)
	data, err = reparsed.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "workflows:")
}
