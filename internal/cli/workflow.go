package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/fetch"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/lock"
	"github.com/fullsend-ai/fullsend/internal/resolve"
	"github.com/fullsend-ai/fullsend/internal/ui"
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

// describeWorkflow renders the run plan's Workflow line:
// <definition> (<commit>, sha256:<hash>) delivered as plugin <namespace>;
// workflow <name>. It names what is uploaded, not a command: this
// release delivers the definition and does not start the workflow.
func describeWorkflow(rw *resolve.ResolvedWorkflow) string {
	pin := fmt.Sprintf("%s, sha256:%s", shortHash(rw.Commit), shortHash(rw.TreeHash))
	if rw.Local {
		pin = fmt.Sprintf("local %s, sha256:%s", rw.Source, shortHash(rw.TreeHash))
	}
	return fmt.Sprintf("%s (%s) delivered as plugin %s; workflow %s", rw.Definition, pin, rw.PluginName, rw.Name)
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
