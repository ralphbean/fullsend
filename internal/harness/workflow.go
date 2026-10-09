package harness

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// WorkflowSpec is the harness workflow: field (ADR 0130). It names a
// workflow-definition repository pinned in config.yaml workflows:
// (Definition), the workflow script inside it (Name, the basename of
// workflows/<name>.js) and optional arguments (Args), whose ${VAR}
// references the runner expands from its environment.
type WorkflowSpec struct {
	Definition string `yaml:"definition"`
	Name       string `yaml:"name"`
	Args       string `yaml:"args,omitempty"`
}

// validWorkflowIdent matches workflow.definition and workflow.name: the
// config workflows: name rule and the plugin basename rule.
var validWorkflowIdent = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validateWorkflow is the Validate() check for the workflow: field.
func (h *Harness) validateWorkflow() error {
	return ValidateWorkflowSpec(h.Workflow)
}

// ValidateWorkflowSpec checks the shape of a workflow: field; nil passes.
// Whether the definition exists is known once config.yaml is loaded, at
// resolution.
func ValidateWorkflowSpec(w *WorkflowSpec) error {
	if w == nil {
		return nil
	}
	if w.Definition == "" {
		return fmt.Errorf("workflow.definition is required: name the config.yaml workflows: entry that holds the workflow")
	}
	if !validWorkflowIdent.MatchString(w.Definition) {
		return fmt.Errorf("workflow.definition %q may contain only letters, digits, _ and -, like the config.yaml workflows: names", w.Definition)
	}
	if w.Name == "" {
		return fmt.Errorf("workflow.name is required: give the workflow to start, the <name> of workflows/<name>.js in the definition")
	}
	if !validWorkflowIdent.MatchString(w.Name) {
		return fmt.Errorf("workflow.name %q may contain only letters, digits, _ and -, the <name> of workflows/<name>.js", w.Name)
	}
	if strings.ContainsAny(w.Args, "\x00\r\n") {
		return fmt.Errorf("workflow.args must be a single line: remove NUL, carriage return and newline characters")
	}
	return CheckWorkflowArgsVariables(w.Args, nil)
}

// credentialEnvSuffixes extend reservedPluginEnvSuffixes (_PROXY,
// _API_KEY, _TOKEN) with the other credential-shaped endings. A bare
// _KEY is not one: ISSUE_KEY names a Jira work item, which args exist to
// carry.
var credentialEnvSuffixes = []string{"_PASSWORD", "_CREDENTIALS", "_PRIVATE_KEY", "_ACCESS_KEY", "_SECRET_KEY"}

// CredentialShapedEnvName reports whether an environment variable name
// looks like it holds a credential, and the rule it matched: the
// reservedPluginEnvKey suffix and *_SECRET* families plus *_PASSWORD,
// *_CREDENTIALS, *_KEY and OTEL_*, whose exporter headers carry collector
// tokens. A name equal to a suffix without its underscore (TOKEN,
// PASSWORD) matches too. Names are compared case-insensitively.
func CredentialShapedEnvName(name string) (string, bool) {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "OTEL_") {
		return "the OTEL_* family, whose exporter headers carry collector tokens", true
	}
	for _, suffix := range append(append([]string(nil), reservedPluginEnvSuffixes...), credentialEnvSuffixes...) {
		if strings.HasSuffix(upper, suffix) || upper == suffix[1:] {
			return "the *" + suffix + " family (credential-shaped names)", true
		}
	}
	if strings.Contains(upper, "_SECRET") || upper == "SECRET" {
		return "the *_SECRET* family (credential-shaped names)", true
	}
	return "", false
}

// CheckWorkflowArgsVariables refuses workflow.args when a variable the
// runner would expand in it names a credential: args reach the model
// prompt, the run plan, metrics.json and traces, so no credential may get
// in. The runner expands args only when they contain "${", so args
// without it name no variable. extra, when set, adds the runner's own
// denied names (runner-only credentials) with the rule to report.
func CheckWorkflowArgsVariables(args string, extra func(name string) (string, bool)) error {
	if !strings.Contains(args, "${") {
		return nil
	}
	var refused error
	os.Expand(args, func(name string) string {
		if refused != nil {
			return ""
		}
		rule, denied := CredentialShapedEnvName(name)
		if !denied && extra != nil {
			rule, denied = extra(name)
		}
		if denied {
			refused = fmt.Errorf("workflow.args references ${%s}, which names a credential (%s); pass work-item identifiers such as ${ISSUE_NUMBER} instead", name, rule)
		}
		return ""
	})
	return refused
}
