package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/fetch"
	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/internal/gitfetch"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/lock"
	"github.com/fullsend-ai/fullsend/internal/pluginformat"
	"github.com/fullsend-ai/fullsend/internal/security"
)

// WorkflowDependencyField is the Dependency.Field (and lock.yaml field)
// of a remote workflow definition. A lock file that records it is written
// as version 2 (see lock.Save).
const WorkflowDependencyField = lock.WorkflowField

// ResolvedWorkflow is the workflow-definition repository a harness
// workflow: field names (ADR 0130), fetched or read, verified and cached
// as a Claude plugin directory.
type ResolvedWorkflow struct {
	// Definition is the config.yaml workflows: name. It is also the
	// basename of LocalPath, so the plugin's sandbox directory.
	Definition string
	// Name and Args are the harness workflow.name and workflow.args.
	Name string
	Args string
	// Source is the workflows: source without its #sha256= fragment.
	Source string
	// Local is true for a path source inside the repository.
	Local bool
	// Commit is the pinned commit sha of a remote source.
	Commit string
	// TreeHash is the tree hash of the cached (materialized) tree.
	TreeHash string
	// LocalPath is the cache directory, named after Definition.
	LocalPath string
	// PluginName is the namespace Claude Code gives the definition's
	// workflows: the "name" in .claude-plugin/plugin.json, or, without
	// that file, the plugin directory name (Definition).
	PluginName string
}

// Command returns the slash command that starts the workflow:
// /<plugin>:<name>, followed by the literal args when set.
func (r *ResolvedWorkflow) Command() string {
	cmd := "/" + r.PluginName + ":" + r.Name
	if r.Args != "" {
		cmd += " " + r.Args
	}
	return cmd
}

// ResolveWorkflowDefinition resolves h.Workflow against the config.yaml
// workflows: entries and delivers the definition as a Claude plugin: it
// appends the cached directory to h.Plugins, so bootstrap uploads it, the
// injection scan reads it and Claude Code gets it as --plugin-dir. It
// returns nil results when the harness has no workflow: field.
//
// A remote source is checked against opts.OrgAllowlist, looked up in the
// content-addressed cache by its pinned tree hash and otherwise fetched
// with symlinks materialized (opts.TreeFetcher replaces the fetcher in
// tests), hash-checked and cached; the returned Dependency (Field
// "workflow") is what `fullsend lock` records. A local source is read
// from the git index of the checkout that holds fullsendDir (tracked
// files only, see gitfetch.ReadLocalTree) with the same link rules and
// cached by its computed tree hash; it has no Dependency. Both are cached
// under fetch.MaterializedCachePath, a namespace of their own that skills
// and plugins never read or write.
//
// It runs as its own step rather than inside ResolveHarness because the
// pin lives in config.yaml, not in the harness, so the harness-only
// HasURLReferences gate cannot see it; the cache lookup makes a lock
// replay need no special handling.
func ResolveWorkflowDefinition(ctx context.Context, h *harness.Harness, entries []config.WorkflowEntry, fullsendDir string, opts ResolveOpts) (*ResolvedWorkflow, *Dependency, error) {
	spec := h.Workflow
	if spec == nil {
		return nil, nil, nil
	}
	if err := config.ValidateWorkflowEntries(entries, opts.OrgAllowlist); err != nil {
		return nil, nil, fmt.Errorf("config.yaml %w", err)
	}
	index := -1
	for i, w := range entries {
		if strings.EqualFold(w.Name, spec.Definition) {
			index = i
			break
		}
	}
	if index < 0 {
		if len(entries) == 0 {
			return nil, nil, fmt.Errorf("workflow.definition %q: config.yaml has no workflows: entries; pin the definition there under workflows: with a name and a source", spec.Definition)
		}
		return nil, nil, fmt.Errorf("workflow.definition %q is not a config.yaml workflows: name (available: %s); add it under workflows: or use one of those names", spec.Definition, strings.Join(config.WorkflowNames(entries), ", "))
	}
	entry := entries[index]
	label := fmt.Sprintf("workflows[%d] %q", index, entry.Name)

	rw := &ResolvedWorkflow{Definition: entry.Name, Name: spec.Name, Args: spec.Args}
	var dep *Dependency
	var treePath string
	var err error
	if entry.IsRemote() {
		dep, treePath, err = fetchRemoteWorkflow(ctx, label, entry, opts)
		if err != nil {
			return nil, nil, err
		}
		rw.Source = dep.URL
		rw.TreeHash = dep.SHA256
		if info, parseErr := forge.ParseForgeURL(dep.URL); parseErr == nil {
			rw.Commit = info.Ref
		}
	} else {
		rw.Local = true
		rw.Source = entry.Source
		rw.TreeHash, treePath, err = readLocalWorkflow(ctx, label, entry, fullsendDir, opts.WorkspaceRoot)
		if err != nil {
			return nil, nil, err
		}
	}

	// Name the cached tree after the workflows: entry: the sandbox plugin
	// directory takes its name from the path's basename.
	namedPath, err := fetch.CacheNamedSymlink(treePath, entry.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: naming cached directory: %w", label, err)
	}
	rw.LocalPath = namedPath
	if dep != nil {
		dep.LocalPath = namedPath
	}

	if err := checkWorkflowDefinition(rw, entry); err != nil {
		return nil, nil, err
	}
	if err := CheckWorkflowNamespace(h, rw); err != nil {
		return nil, nil, err
	}
	// The cache writes files 0600; a definition's scripts need the
	// executable bit, as fetched plugins do.
	if err := harness.ChmodPluginDir(namedPath); err != nil {
		return nil, nil, fmt.Errorf("%s: setting permissions: %w", label, err)
	}
	h.Plugins = append(h.Plugins, harness.PluginSpec{Path: namedPath})
	return rw, dep, nil
}

// CheckWorkflowNamespace refuses a harness plugins: entry that would load
// as the definition's sandbox plugin directory or under its Claude Code
// plugin name. The definition's own entry (rw.LocalPath) is skipped. A URL
// plugin is checked only once it is resolved, so a caller that resolves
// plugins after ResolveWorkflowDefinition (fullsend lock) calls it again
// then, and refuses what fullsend run refuses.
func CheckWorkflowNamespace(h *harness.Harness, rw *ResolvedWorkflow) error {
	for i, p := range h.Plugins {
		if p.Path == rw.LocalPath {
			continue
		}
		if strings.EqualFold(pluginDirName(p), rw.Definition) {
			return fmt.Errorf("workflows %q: plugins[%d] %q also loads as the sandbox plugin directory %q; rename that plugin directory or the workflows: entry", rw.Definition, i, p.Path, rw.Definition)
		}
		// Claude Code namespaces commands by plugin name, so two plugins
		// with the same name would clash on /<name>:.
		if ns, ok := pluginClaudeNamespace(p); ok && strings.EqualFold(ns, rw.PluginName) {
			return fmt.Errorf("workflows %q: plugins[%d] %q has the Claude Code plugin name %q, the same as the definition's plugin name %q; rename one of them (the \"name\" in %s, or the directory when there is no manifest)", rw.Definition, i, p.Path, ns, rw.PluginName, claudeManifest)
		}
	}
	return nil
}

func fetchRemoteWorkflow(ctx context.Context, label string, entry config.WorkflowEntry, opts ResolveOpts) (*Dependency, string, error) {
	cleanURL, expectedHash, _ := harness.ParseIntegrityHash(entry.Source)
	info, err := forge.ParseForgeURL(cleanURL)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", label, err)
	}
	allowedBy := harness.MatchingAllowedPrefixInList(cleanURL, opts.OrgAllowlist)

	treePath, dirEntry, err := fetch.CacheGetMaterializedDir(opts.WorkspaceRoot, expectedHash)
	if err != nil {
		return nil, "", fmt.Errorf("%s: cache lookup: %w", label, err)
	}
	cacheHit := treePath != ""
	fetchedAt := time.Now().UTC()
	if cacheHit {
		fetchedAt = dirEntry.FetchTime
	} else {
		if opts.FetchPolicy.Offline {
			return nil, "", fmt.Errorf("%s: %s is not in the cache and offline mode forbids fetching it; run without --offline or `fullsend lock` first", label, cleanURL)
		}
		fetcher := opts.TreeFetcher
		if fetcher == nil {
			fetcher = materializingFetcher
		}
		files, fetchErr := fetcher(ctx, info.CloneURL(), info.Path, info.Ref, opts.GitToken)
		if fetchErr != nil {
			var linkErr *gitfetch.LinkError
			if errors.As(fetchErr, &linkErr) {
				return nil, "", fmt.Errorf("%s: %w", label, linkErr)
			}
			// git's stderr is external text: mask credentials in it, as
			// the runner does for other external output.
			if res := security.NewSecretRedactor().Scan(fetchErr.Error()); !res.Safe {
				fetchErr = &redactedError{msg: res.Sanitized, err: fetchErr}
			}
			if opts.GitToken == "" {
				return nil, "", fmt.Errorf("%s: fetching %s: %w (hint: set GH_TOKEN or GITHUB_TOKEN for private repos)", label, cleanURL, fetchErr)
			}
			return nil, "", fmt.Errorf("%s: fetching %s: %w", label, cleanURL, fetchErr)
		}
		if actual := fetch.ComputeTreeHash(files); actual != expectedHash {
			return nil, "", fmt.Errorf("%s: tree hash mismatch for %s: config.yaml pins sha256=%s but the fetched tree hashes to sha256=%s; if the commit is the one you meant, update the #sha256= fragment", label, cleanURL, expectedHash, actual)
		}
		if _, err := fetch.CachePutMaterializedDir(opts.WorkspaceRoot, cleanURL, files); err != nil {
			return nil, "", fmt.Errorf("%s: caching: %w", label, err)
		}
		cachePath, err := fetch.MaterializedCachePath(opts.WorkspaceRoot, expectedHash)
		if err != nil {
			return nil, "", fmt.Errorf("%s: computing cache path: %w", label, err)
		}
		treePath = filepath.Join(cachePath, "tree")
	}

	if opts.AuditLogPath != "" {
		if err := fetch.AppendFetchAudit(opts.AuditLogPath, fetch.FetchAuditEntry{
			TraceID:   opts.TraceID,
			FetchTime: fetchedAt,
			URL:       cleanURL,
			SHA256:    expectedHash,
			FetchType: "static",
			AllowedBy: allowedBy,
			CacheHit:  cacheHit,
		}); err != nil {
			return nil, "", fmt.Errorf("writing fetch audit log: %w", err)
		}
	}
	return &Dependency{
		Field:     WorkflowDependencyField,
		URL:       cleanURL,
		LocalPath: treePath,
		SHA256:    expectedHash,
		FetchedAt: fetchedAt,
		CacheHit:  cacheHit,
		Type:      "directory",
	}, treePath, nil
}

// materializingFetcher is the default remote fetcher for definitions:
// the tree fetcher with symlinks materialized (ADR 0130 rule 2).
func materializingFetcher(ctx context.Context, cloneURL, subpath, ref, token string) (map[string][]byte, error) {
	return gitfetch.FetchTreeWithOptions(ctx, cloneURL, subpath, ref, token, gitfetch.Options{MaterializeSymlinks: true})
}

// pluginDirName is the sandbox directory name of a plugins: entry: the
// basename of its local path, or, for a URL entry not resolved yet (at lock
// time, before ResolveHarness), the basename of the URL's tree path.
func pluginDirName(p harness.PluginSpec) string {
	if harness.IsURL(p.Path) {
		cleanURL, _, _ := harness.ParseIntegrityHash(p.Path)
		if info, err := forge.ParseForgeURL(cleanURL); err == nil && info.Path != "" {
			return path.Base(info.Path)
		}
	}
	return p.Name()
}

// pluginClaudeNamespace is the name Claude Code gives a plugins: entry
// that the Claude runtime loads: the exact "name" key in its
// .claude-plugin/plugin.json, or its directory name when the manifest is
// absent, unreadable or names nothing. ok is false when the entry is not a
// Claude plugin on disk yet: a URL entry before resolution (it is checked
// at run time, once resolved) or a directory the Claude runtime skips,
// such as a pi extension.
func pluginClaudeNamespace(p harness.PluginSpec) (ns string, ok bool) {
	if harness.IsURL(p.Path) {
		return "", false
	}
	if kind, _, err := pluginformat.Detect(p.Path); err != nil || kind != pluginformat.KindClaude {
		return "", false
	}
	dirName := pluginDirName(p)
	data, err := os.ReadFile(filepath.Join(p.Path, filepath.FromSlash(claudeManifest)))
	if err != nil {
		return dirName, true
	}
	// Decode into a map: a struct would also take "Name" or "NAME", since
	// encoding/json matches field names case-insensitively, while Claude
	// Code reads only "name".
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return dirName, true
	}
	var name string
	if raw, found := m["name"]; !found || json.Unmarshal(raw, &name) != nil || name == "" {
		return dirName, true
	}
	return name, true
}

// readLocalWorkflow reads a local definition from the git index of the
// checkout that holds fullsendDir into the content-addressed cache, so the
// uploaded directory holds only tracked files, no symlinks, and is the
// tree that was hashed. It returns the tree hash and the cached tree path.
func readLocalWorkflow(ctx context.Context, label string, entry config.WorkflowEntry, fullsendDir, workspaceRoot string) (string, string, error) {
	if fullsendDir == "" {
		return "", "", fmt.Errorf("%s: local source %q needs the repository that holds .fullsend/, which is not known here", label, entry.Source)
	}
	top, err := gitfetch.RepoToplevel(ctx, fullsendDir)
	if err != nil {
		return "", "", fmt.Errorf("%s: a local workflows: source needs the repository that holds .fullsend/ to be a git checkout, and %q is not inside one (%w); run from a git checkout or pin a remote source", label, fullsendDir, err)
	}
	exclude, err := localWorkflowExclude(label, entry.Source, top, fullsendDir)
	if err != nil {
		return "", "", err
	}
	files, err := gitfetch.ReadLocalTree(ctx, top, entry.Source, exclude)
	if err != nil {
		return "", "", fmt.Errorf("%s: local source %q: %w", label, entry.Source, err)
	}
	treeHash := fetch.ComputeTreeHash(files)
	treePath, _, err := fetch.CacheGetMaterializedDir(workspaceRoot, treeHash)
	if err != nil {
		return "", "", fmt.Errorf("%s: cache lookup: %w", label, err)
	}
	if treePath != "" {
		return treeHash, treePath, nil
	}
	if _, err := fetch.CachePutMaterializedDir(workspaceRoot, entry.Source, files); err != nil {
		return "", "", fmt.Errorf("%s: caching: %w", label, err)
	}
	cachePath, err := fetch.MaterializedCachePath(workspaceRoot, treeHash)
	if err != nil {
		return "", "", fmt.Errorf("%s: computing cache path: %w", label, err)
	}
	return treeHash, filepath.Join(cachePath, "tree"), nil
}

// localWorkflowExclude keeps fullsend's own configuration directory out of
// a local definition (ADR 0130 rule 2): it returns the directory's path
// relative to the checkout top, for ReadLocalTree to leave out when it lies
// under the source. A source that is the configuration directory or lies
// inside it is refused. When the configuration directory is the checkout
// top (a configuration repository), only `.` is refused: every
// sub-directory is outside the configuration files at the top, and nothing
// is left out of it.
func localWorkflowExclude(label, source, top, fullsendDir string) ([]string, error) {
	realFullsend, err := filepath.EvalSymlinks(fullsendDir)
	if err != nil {
		return nil, fmt.Errorf("%s: resolving %s: %w", label, fullsendDir, err)
	}
	rel, err := filepath.Rel(top, realFullsend)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// The configuration directory is outside the checkout, so no
		// source can include it.
		return nil, nil
	}
	rel = filepath.ToSlash(rel)
	clean := path.Clean(source)
	if rel == "." {
		if clean == "." {
			return nil, fmt.Errorf("%s: source %q is the top of a checkout that is fullsend's configuration directory, which is never part of a definition; put the definition in a sub-directory and set source to its path", label, source)
		}
		return nil, nil
	}
	if clean == rel {
		return nil, fmt.Errorf("%s: source %q is fullsend's configuration directory, which is never part of a definition; put the definition in another directory and set source to its path", label, source)
	}
	if strings.HasPrefix(clean, rel+"/") {
		return nil, fmt.Errorf("%s: source %q is inside fullsend's configuration directory %s/, which is never part of a definition; put the definition in another directory and set source to its path", label, source, rel)
	}
	return []string{rel}, nil
}

// claudeManifest is Claude Code's plugin manifest. Claude Code reads the
// plugin name only from this file; fullsend's root plugin.json marks a
// Claude plugin for fullsend but Claude Code does not read it.
const claudeManifest = ".claude-plugin/plugin.json"

// checkWorkflowDefinition checks the cached definition: it is a Claude
// plugin, its namespace is usable (recorded on rw), and it ships
// workflows/<rw.Name>.js.
func checkWorkflowDefinition(rw *ResolvedWorkflow, entry config.WorkflowEntry) error {
	kind, _, err := pluginformat.Detect(rw.LocalPath)
	if err != nil {
		return fmt.Errorf("workflows %q: %w", entry.Name, err)
	}
	if kind != pluginformat.KindClaude {
		return fmt.Errorf("workflows %q: %s is not a Claude Code plugin: add .claude-plugin/plugin.json at the definition root", entry.Name, rw.Source)
	}

	name, err := pluginNamespace(rw.LocalPath, entry.Name)
	if err != nil {
		return fmt.Errorf("workflows %q: %w", entry.Name, err)
	}
	rw.PluginName = name

	available, err := listWorkflowScripts(filepath.Join(rw.LocalPath, "workflows"))
	if err != nil {
		return fmt.Errorf("workflows %q: listing workflows/: %w", entry.Name, err)
	}
	for _, a := range available {
		if a == rw.Name {
			return checkWorkflowMeta(rw, entry.Name)
		}
	}
	if len(available) == 0 {
		return fmt.Errorf("workflows %q: has no workflows/%s.js, and no workflows/*.js scripts at all; the harness workflow.name must name one", entry.Name, rw.Name)
	}
	return fmt.Errorf("workflows %q: has no workflows/%s.js; the definition ships: %s", entry.Name, rw.Name, strings.Join(available, ", "))
}

// pluginNamespace returns the namespace Claude Code gives the plugin at
// dir when it is loaded with --plugin-dir: the "name" in
// .claude-plugin/plugin.json, or, without that file, the directory name,
// which is the workflows: entry name. It refuses a manifest that sets the
// "workflows" key, which moves the scripts away from workflows/.
func pluginNamespace(dir, dirName string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(claudeManifest)))
	if errors.Is(err, os.ErrNotExist) {
		return dirName, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", claudeManifest, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("%s is not valid JSON: %w; fix the plugin manifest", claudeManifest, err)
	}
	if _, ok := m["workflows"]; ok {
		return "", fmt.Errorf("%s sets \"workflows\", a custom workflow path, which fullsend does not support yet; keep the scripts in workflows/ at the definition root and remove the \"workflows\" key", claudeManifest)
	}
	var name string
	if raw, ok := m["name"]; ok {
		if err := json.Unmarshal(raw, &name); err != nil {
			return "", fmt.Errorf("%s \"name\" is not a string; set it to letters, digits, _ and -", claudeManifest)
		}
	}
	if !config.ValidWorkflowName(name) {
		return "", fmt.Errorf("%s has no usable \"name\" (got %q); set \"name\" to letters, digits, _ and -, since it is the /<name>: prefix of the workflow command", claudeManifest, name)
	}
	return name, nil
}

// listWorkflowScripts returns the sorted workflow names in dir: regular
// files named <name>.js with a valid name. The directory is read as a
// literal path, so glob characters in the workspace path do not matter.
func listWorkflowScripts(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".js")
		if !ok || !e.Type().IsRegular() || !config.ValidWorkflowName(base) {
			continue
		}
		names = append(names, base)
	}
	sort.Strings(names)
	return names, nil
}

// redactedError shows a redacted message but keeps the original error in
// its chain, so errors.Is and errors.As (a transient fetch failure, a
// cancelled context) still classify it.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
