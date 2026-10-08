package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/internal/urlutil"
)

// WorkflowEntry pins a workflow-definition repository (ADR 0130): a Claude
// Code plugin that ships workflows/<name>.js scripts. A harness names the
// entry in its workflow: field; the runner fetches the source and delivers
// it to the sandbox as a Claude plugin.
//
// Source is either a GitHub tree URL at a full commit sha with a
// #sha256=<tree hash> fragment, at the repository root or a sub-directory,
// or a path inside the repository that holds the .fullsend/ directory
// ("." is that repository's root).
type WorkflowEntry struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
}

// IsRemote reports whether the entry's source is a URL rather than a path
// inside the repository.
func (w WorkflowEntry) IsRemote() bool { return urlutil.IsURL(w.Source) }

// validWorkflowName is the sandbox plugin directory name the definition
// is uploaded under, so it follows the plugin basename rule
// (harness.ValidPluginBasename; config cannot import harness).
var validWorkflowName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// reservedWorkflowNames are the lower-cased entry names a definition may
// not take: the entry name becomes a link beside the cache-internal
// entries of the definition's cache directory (fetch.CacheNamedSymlink),
// and on a case-insensitive file system "Tree" is "tree".
var reservedWorkflowNames = map[string]bool{"tree": true, "content": true}

// validCommitSHA matches a full 40-hex-character git commit sha.
var validCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// ValidWorkflowName reports whether name is a valid workflows: entry name.
func ValidWorkflowName(name string) bool { return validWorkflowName.MatchString(name) }

// ValidateWorkflowEntries validates a complete workflows: list against the
// resolved allowed_remote_resources. The run and lock paths call it on the
// merged list, because loading a config does not validate it.
func ValidateWorkflowEntries(entries []WorkflowEntry, allowlist []string) error {
	return validateWorkflowEntries(entries, allowlist, true)
}

// validateWorkflowEntries is ValidateWorkflowEntries with the allowlist
// check made conditional, for repos.yaml managed layers validated before
// their allowed_remote_resources shorthand is applied (see
// validateAgentEntries).
func validateWorkflowEntries(entries []WorkflowEntry, allowlist []string, enforceAllowlist bool) error {
	seen := make(map[string]int, len(entries))
	for i, w := range entries {
		if w.Name == "" {
			return fmt.Errorf("workflows[%d]: name is required; give the definition a name (letters, digits, _ and -) that harnesses reference in workflow.definition", i)
		}
		if !validWorkflowName.MatchString(w.Name) {
			return fmt.Errorf("workflows[%d] %q: name may contain only letters, digits, _ and -, because it names the plugin directory in the sandbox", i, w.Name)
		}
		lower := strings.ToLower(w.Name)
		if reservedWorkflowNames[lower] {
			return fmt.Errorf("workflows[%d] %q: name is reserved, because the definition's cache directory already holds an entry called %q (names are compared without case); choose another name", i, w.Name, lower)
		}
		if prev, ok := seen[lower]; ok {
			return fmt.Errorf("workflows[%d] %q: name is already used by workflows[%d] (names are compared without case); give each definition its own name", i, w.Name, prev)
		}
		seen[lower] = i
		if err := validateWorkflowSource(i, w, allowlist, enforceAllowlist); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkflowSource(i int, w WorkflowEntry, allowlist []string, enforceAllowlist bool) error {
	label := fmt.Sprintf("workflows[%d] %q", i, w.Name)
	src := w.Source
	if src == "" {
		return fmt.Errorf("%s: source is required; give a GitHub tree URL pinned to a commit sha with #sha256=<tree hash>, or a path inside this repository", label)
	}
	if strings.ContainsAny(src, "\x00\r\n") {
		return fmt.Errorf("%s: source must not contain NUL, carriage return or newline characters", label)
	}
	if urlutil.IsURL(src) {
		return validateRemoteWorkflowSource(label, src, allowlist, enforceAllowlist)
	}
	lower := strings.ToLower(src)
	if strings.HasPrefix(lower, "http://") {
		return fmt.Errorf("%s: source URL must use https, got http", label)
	}
	if strings.Contains(src, "://") {
		return fmt.Errorf("%s: source URL scheme is not supported; use an https GitHub tree URL or a path inside this repository", label)
	}
	if strings.HasPrefix(src, "/") {
		return fmt.Errorf("%s: source %q is an absolute path; give a path relative to the repository that holds .fullsend/ (\".\" is its root)", label, src)
	}
	if strings.ContainsRune(src, '\\') {
		return fmt.Errorf("%s: source %q contains a backslash; separate path segments with /", label, src)
	}
	for _, seg := range strings.Split(src, "/") {
		switch seg {
		case "..":
			return fmt.Errorf("%s: source %q contains \"..\"; a local definition must be inside the repository that holds .fullsend/", label, src)
		case ".git", ".fullsend-cache":
			return fmt.Errorf("%s: source %q names %s/, which is never part of a definition; point source at the definition's directory in the working tree", label, src, seg)
		}
	}
	return nil
}

func validateRemoteWorkflowSource(label, src string, allowlist []string, enforceAllowlist bool) error {
	cleanURL, _, hasHash := urlutil.ParseIntegrityHash(src)
	if !hasHash {
		return fmt.Errorf("%s: source URL must end in #sha256=<64 hex characters>, the tree hash of the fetched definition, so the runner can verify what it uploads", label)
	}
	info, err := forge.ParseForgeURL(cleanURL)
	if err != nil {
		return fmt.Errorf("%s: source must be a GitHub tree URL of the form https://github.com/<owner>/<repo>/tree/<commit sha>[/<path>]: %w", label, err)
	}
	if info.Forge != "github" {
		return fmt.Errorf("%s: source is on %q, but a remote workflow definition must be a github.com tree URL today, because tree fetching (shared with plugins: and skills:) supports only github.com; use a local path source, which works on any forge", label, info.Forge)
	}
	if info.PathType != "tree" {
		return fmt.Errorf("%s: source URL must use /tree/ (a directory), not /%s/", label, info.PathType)
	}
	if !validCommitSHA.MatchString(info.Ref) {
		return fmt.Errorf("%s: source ref %q is not a commit sha; pin the commit sha (40 hex characters) so the definition cannot move under the pin", label, info.Ref)
	}
	if info.Path != "" {
		for _, seg := range strings.Split(info.Path, "/") {
			if seg == "." || seg == ".." {
				return fmt.Errorf("%s: source path %q must not contain \".\" or \"..\" segments", label, info.Path)
			}
		}
	}
	if enforceAllowlist && urlutil.MatchingAllowedPrefixInList(cleanURL, allowlist) == "" {
		return fmt.Errorf("%s: source URL %q is not covered by allowed_remote_resources; add a prefix such as https://github.com/%s/%s/ to allowed_remote_resources in config.yaml", label, cleanURL, info.Owner, info.Repo)
	}
	return nil
}

// mergeWorkflowEntries layers child over parent keyed by name (compared
// without case): a child entry replaces the parent entry of the same name
// in place, and child entries naming no parent entry are appended in
// their own order. A name the child lists twice stays listed twice, so
// validation still reports the duplicate.
func mergeWorkflowEntries(parent, child []WorkflowEntry) []WorkflowEntry {
	if len(parent) == 0 {
		return child
	}
	first := make(map[string]int, len(child))
	for i, w := range child {
		key := strings.ToLower(w.Name)
		if _, ok := first[key]; !ok {
			first[key] = i
		}
	}
	consumed := make(map[int]bool, len(child))
	out := make([]WorkflowEntry, 0, len(parent)+len(child))
	for _, p := range parent {
		if i, ok := first[strings.ToLower(p.Name)]; ok && !consumed[i] {
			out = append(out, child[i])
			consumed[i] = true
			continue
		}
		out = append(out, p)
	}
	for i, w := range child {
		if !consumed[i] {
			out = append(out, w)
		}
	}
	return out
}

// FindWorkflowEntry returns the entry named name (compared without case)
// and whether one exists.
func FindWorkflowEntry(entries []WorkflowEntry, name string) (WorkflowEntry, bool) {
	lower := strings.ToLower(name)
	for _, w := range entries {
		if strings.ToLower(w.Name) == lower {
			return w, true
		}
	}
	return WorkflowEntry{}, false
}

// WorkflowNames returns the entry names in order, for error messages.
func WorkflowNames(entries []WorkflowEntry) []string {
	names := make([]string, 0, len(entries))
	for _, w := range entries {
		names = append(names, w.Name)
	}
	return names
}
