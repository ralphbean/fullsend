package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/fetch"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/lock"
	"github.com/fullsend-ai/fullsend/internal/resolve"
	agentruntime "github.com/fullsend-ai/fullsend/internal/runtime"
	"github.com/fullsend-ai/fullsend/internal/security"
	"github.com/fullsend-ai/fullsend/internal/ui"

	"go.opentelemetry.io/otel/attribute"
)

// checkWorkflowRuntime refuses a harness workflow: field under a runtime
// that cannot start a Claude Code workflow (ADR 0130 rule 4). The dummy
// runtimes pass so behaviour tests can run a workflow harness without a
// model.
func checkWorkflowRuntime(h *harness.Harness, agentName, runtimeName string) error {
	if h.Workflow == nil {
		return nil
	}
	switch runtimeName {
	case "claude", "dummy", "dummy-playback":
		return nil
	}
	return fmt.Errorf("workflow: needs the claude runtime; agent %q resolves to %q; set runtime: claude for this agent or remove workflow: from its harness", agentName, runtimeName)
}

// resolveHarnessWorkflow resolves h.Workflow against the config.yaml
// workflows: entries; local sources are read from the git checkout that
// holds absFullsendDir. cfg may be nil (no config.yaml).
func resolveHarnessWorkflow(ctx context.Context, h *harness.Harness, cfg config.ConfigReader, absFullsendDir string, opts resolve.ResolveOpts) (*resolve.ResolvedWorkflow, *resolve.Dependency, error) {
	if h.Workflow == nil {
		return nil, nil, nil
	}
	var entries []config.WorkflowEntry
	if cfg != nil {
		entries = cfg.WorkflowEntries()
	}
	return resolve.ResolveWorkflowDefinition(ctx, h, entries, absFullsendDir, opts)
}

// describeWorkflow renders the run plan's Workflow line: the definition,
// its pin and the command the runner starts,
// <definition> (<commit>, sha256:<hash>) → /<namespace>:<name>[ <args>].
func describeWorkflow(rw *resolve.ResolvedWorkflow) string {
	pin := fmt.Sprintf("%s, sha256:%s", shortHash(rw.Commit), shortHash(rw.TreeHash))
	if rw.Local {
		pin = fmt.Sprintf("local %s, sha256:%s", rw.Source, shortHash(rw.TreeHash))
	}
	return fmt.Sprintf("%s (%s) → %s", rw.Definition, pin, rw.Command())
}

// workflowToolName is the Claude Code tool that starts a workflow.
const workflowToolName = "Workflow"

// checkWorkflowAgentTools refuses an agent whose frontmatter tools: list
// leaves out Workflow: its main loop could not start the command the
// runner hands it (ADR 0130 rule 5). An agent without a tools: list
// inherits every tool and passes.
func checkWorkflowAgentTools(h *harness.Harness, agentName string, rw *resolve.ResolvedWorkflow) error {
	if rw == nil {
		return nil
	}
	access, err := agentruntime.AgentDefinitionTools(h.Agent)
	if err != nil {
		return fmt.Errorf("workflow: cannot read the tools of agent %s (%s): %w; fix the agent file", agentName, h.Agent, err)
	}
	if slices.Contains(access.Disallowed, workflowToolName) {
		return fmt.Errorf("workflow: agent %s lists Workflow in disallowedTools:, so it cannot start /%s:%s; remove Workflow from its disallowedTools:", agentName, rw.PluginName, rw.Name)
	}
	if access.Listed && !slices.Contains(access.Tools, workflowToolName) {
		return fmt.Errorf("workflow: agent %s lists tools: without Workflow, so it cannot start /%s:%s; add Workflow to its tools:", agentName, rw.PluginName, rw.Name)
	}
	return nil
}

// workflowFeedbackNote is printed once per run when a validation-loop
// retry of a workflow harness has feedback it does not pass on.
const workflowFeedbackNote = "The workflow restarts from its own state; validation feedback is not passed to it"

// iterationPrompt chooses the agent prompt of one iteration. A workflow
// harness starts rw.Command() on every iteration; a retry with
// feedback_mode: append feedback leaves the command as it is, and note
// reports that once per run (noted carries that across iterations). Other
// harnesses get the default prompt, or the feedback prompt on such a
// retry (inject is true then).
func iterationPrompt(rw *resolve.ResolvedWorkflow, iteration int, feedbackEnabled bool, feedback string, noted *bool) (prompt string, sanitized int, inject, note bool) {
	retryFeedback := iteration > 1 && feedbackEnabled && feedback != ""
	if rw != nil {
		if retryFeedback && !*noted {
			*noted = true
			note = true
		}
		return rw.Command(), 0, false, note
	}
	if retryFeedback {
		prompt, sanitized = buildFeedbackPrompt(feedback)
		return prompt, sanitized, true, false
	}
	return "", 0, false, false
}

// workflowLaunch is the metrics.json record of the workflow a run started:
// written for every runtime, so a dummy run, which runs no model, still
// records what the runner launched (ADR 0130 rule 4).
type workflowLaunch struct {
	Definition string `json:"definition"`
	// Source is the workflows: source without its #sha256= fragment, or
	// the local path.
	Source    string `json:"source"`
	PinSHA256 string `json:"pin_sha256"`
	Command   string `json:"command"`
}

func newWorkflowLaunch(rw *resolve.ResolvedWorkflow) *workflowLaunch {
	if rw == nil {
		return nil
	}
	return &workflowLaunch{Definition: rw.Definition, Source: rw.Source, PinSHA256: rw.TreeHash, Command: rw.Command()}
}

// workflowSpanAttr is the agent span's fullsend.workflow attribute: the
// /<namespace>:<name> the runner starts, without args, which stay out of
// traces.
func workflowSpanAttr(rw *resolve.ResolvedWorkflow) attribute.KeyValue {
	return boundedStringAttr("fullsend.workflow", "/"+rw.PluginName+":"+rw.Name)
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// cachedDirFor looks a locked directory dependency up in its cache
// namespace: the workflow definition in the materialized-tree namespace,
// every other field in the shared one.
func cachedDirFor(field, workspaceRoot, hash string) (string, *fetch.DirCacheEntry, error) {
	if field == resolve.WorkflowDependencyField {
		return fetch.CacheGetMaterializedDir(workspaceRoot, hash)
	}
	return fetch.CacheGetDir(workspaceRoot, hash)
}

// lockDepKey is the key lockOneAgent deduplicates dependencies by: the
// URL, so a tree that sits at different skills[N] indices in two forge
// variants is locked once, except for the workflow field, which is keyed
// by field and URL. The same tree may back both the workflow definition
// and a plugins: or skills: entry, and lock replay needs both bindings.
func lockDepKey(field, url string) string {
	if field == resolve.WorkflowDependencyField {
		return field + "\x00" + url
	}
	return url
}

// workflowPin is a workflow definition's pin as lock.yaml records it: the
// tree URL without its fragment and the tree hash. It is zero for a local
// source, which is not locked.
type workflowPin struct {
	URL, SHA256 string
}

func configWorkflowPin(e config.WorkflowEntry) workflowPin {
	if !e.IsRemote() {
		return workflowPin{}
	}
	url, hash, _ := harness.ParseIntegrityHash(e.Source)
	return workflowPin{URL: url, SHA256: hash}
}

// lockedWorkflowDep returns the lock entry's workflow dependency, or nil.
func lockedWorkflowDep(entry *lock.HarnessLock) *lock.DependencyEntry {
	if entry == nil {
		return nil
	}
	for i := range entry.Dependencies {
		if entry.Dependencies[i].Field == resolve.WorkflowDependencyField {
			return &entry.Dependencies[i]
		}
	}
	return nil
}

// workflowPinDrift compares the effective pin of workflows: entry
// definition with the lock entry and says how they differ, or "".
func workflowPinDrift(definition string, current workflowPin, entry *lock.HarnessLock) string {
	locked := lockedWorkflowDep(entry)
	switch {
	case locked == nil && current.URL == "":
		return ""
	case locked == nil:
		return fmt.Sprintf("config.yaml pins workflows %q to %s#sha256=%s, which lock.yaml does not record", definition, current.URL, current.SHA256)
	case current.URL == "":
		return fmt.Sprintf("workflows %q is a local source in config.yaml, but lock.yaml pins %s#sha256=%s", definition, locked.URL, locked.SHA256)
	case locked.URL != current.URL || locked.SHA256 != current.SHA256:
		return fmt.Sprintf("config.yaml pins workflows %q to %s#sha256=%s, but lock.yaml has %s#sha256=%s", definition, current.URL, current.SHA256, locked.URL, locked.SHA256)
	}
	return ""
}

// lockWorkflowDrift is the workflow part of `fullsend lock`'s freshness
// check for a lock entry whose harness hash still matches: the pin lives
// in config.yaml (after layering), so a changed pin, a replaced or
// removed definition must make the entry stale. It compares the lock
// entry with the pin of the definition the effective harness selects,
// and only that one.
//
// A workflow: field set in the harness itself is the effective one (a
// child replaces its base's workflow: wholesale). Without one, a harness
// with base: may inherit a workflow:, so compose builds the composed
// harness from what is on disk and in the cache. When it fails, recheck
// is true and the caller resolves the harness instead of trusting the
// entry.
func lockWorkflowDrift(harnessPath string, cfg config.ConfigReader, entry *lock.HarnessLock, compose func() (*harness.Harness, error)) (drift string, recheck bool) {
	raw, err := harness.LoadRaw(harnessPath)
	if err != nil {
		// Let the full resolution report the harness problem.
		return "", false
	}
	var entries []config.WorkflowEntry
	if cfg != nil {
		entries = cfg.WorkflowEntries()
	}
	spec := raw.Workflow
	if spec == nil && raw.Base != "" {
		composed, err := compose()
		if err != nil {
			return "", true
		}
		spec = composed.Workflow
	}
	if spec == nil {
		if locked := lockedWorkflowDep(entry); locked != nil {
			return fmt.Sprintf("lock.yaml pins workflow definition %s#sha256=%s, but the harness names no workflow: definition", locked.URL, locked.SHA256), false
		}
		return "", false
	}
	e, ok := config.FindWorkflowEntry(entries, spec.Definition)
	if !ok {
		return fmt.Sprintf("workflow.definition %q is not a config.yaml workflows: name any more", spec.Definition), false
	}
	return workflowPinDrift(e.Name, configWorkflowPin(e), entry), false
}

// composeHarnessOffline composes the harness at harnessPath with its base:
// chain from local files and the cache only, for lockWorkflowDrift: the
// freshness check must not fetch.
func composeHarnessOffline(ctx context.Context, harnessPath, absFullsendDir, forgeFlag string, cfg config.ConfigReader, rFlags resolveFlags) (*harness.Harness, error) {
	platforms, err := lockForgePlatforms(harnessPath, forgeFlag)
	if err != nil {
		return nil, err
	}
	allowlist := config.DefaultAllowedRemoteResources()
	if cfg != nil {
		allowlist = cfg.AllowedResources()
	}
	policy := fetch.DefaultPolicy
	policy.Offline = true
	h, _, err := harness.LoadWithBase(ctx, harnessPath, harness.ComposeOpts{
		WorkspaceRoot: absFullsendDir,
		FetchPolicy:   policy,
		ForgePlatform: platforms[0],
		OrgAllowlist:  allowlist,
		TreeFetcher:   rFlags.treeFetcher,
		Config:        harness.BuildConfigMap(cfg),
	})
	return h, err
}

// warnWorkflowLockDrift warns when the workflow definition a run resolved
// from config.yaml differs from what lock.yaml records for the agent. The
// run uses the config.yaml pin; the warning says how to bring the lock in
// line. Nothing is printed without a lock.yaml.
func warnWorkflowLockDrift(printer *ui.Printer, absFullsendDir, fullsendDir, agentName string, rw *resolve.ResolvedWorkflow, dep *resolve.Dependency) {
	lf, err := lock.Load(filepath.Join(absFullsendDir, "lock.yaml"))
	if err != nil || lf == nil {
		return
	}
	var current workflowPin
	if dep != nil {
		current = workflowPin{URL: dep.URL, SHA256: dep.SHA256}
	}
	entry := lf.Lookup(agentName)
	if entry == nil {
		if current.URL != "" {
			printer.StepWarn(fmt.Sprintf("lock.yaml has no entry for %s, so workflows %q (%s) is not locked. Run 'fullsend lock %s%s' to update.", agentName, rw.Definition, current.URL, agentName, fullsendDirArg(fullsendDir)))
		}
		return
	}
	if drift := workflowPinDrift(rw.Definition, current, entry); drift != "" {
		printer.StepWarn(fmt.Sprintf("Workflow definition differs from lock.yaml: %s; this run uses the config.yaml pin. Run 'fullsend lock %s%s' to update.", drift, agentName, fullsendDirArg(fullsendDir)))
	}
}

// expandWorkflowArgs expands ${VAR} references in workflow.args with the
// runner's harness expander. Args reach the model prompt, the run plan,
// metrics.json and traces, so it refuses, before expanding, a reference
// to a credential-shaped or runner-only variable, and, after expanding, a
// result that looks like a credential (the value is not printed). It also
// refuses a result that is no longer a single line: an expanded value
// must not add a second line to the slash command the runner starts.
func expandWorkflowArgs(args string, expand func(string) string) (string, error) {
	if err := harness.CheckWorkflowArgsVariables(args, runnerOnlyCredential); err != nil {
		return "", err
	}
	out := os.Expand(args, expand)
	if strings.ContainsAny(out, "\x00\r\n") {
		return "", fmt.Errorf("workflow.args %q expands to more than one line; the variables it names must not hold NUL, carriage return or newline characters", args)
	}
	if res := security.NewSecretRedactor().Scan(out); !res.Safe {
		return "", fmt.Errorf("workflow.args expands to a value that looks like a credential; pass work-item identifiers only")
	}
	return out, nil
}

// runnerOnlyCredential is harnessExpansionDenied in the shape
// harness.CheckWorkflowArgsVariables takes.
func runnerOnlyCredential(name string) (string, bool) {
	if harnessExpansionDenied(name) {
		return "a runner-only credential no harness may expand", true
	}
	return "", false
}
