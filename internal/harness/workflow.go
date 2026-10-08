package harness

import (
	"fmt"
	"regexp"
	"strings"
)

// WorkflowSpec is the harness workflow: field (ADR 0130). It names a
// workflow-definition repository pinned in config.yaml workflows:
// (Definition), the workflow script inside it (Name, the basename of
// workflows/<name>.js) and optional literal arguments (Args).
type WorkflowSpec struct {
	Definition string `yaml:"definition"`
	Name       string `yaml:"name"`
	Args       string `yaml:"args,omitempty"`
}

// validWorkflowIdent matches workflow.definition and workflow.name: the
// config workflows: name rule and the plugin basename rule.
var validWorkflowIdent = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validateWorkflow is the Validate() check for the workflow: field. It
// checks shape only; whether the definition exists is known once
// config.yaml is loaded, at resolution.
func (h *Harness) validateWorkflow() error {
	w := h.Workflow
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
	return nil
}
