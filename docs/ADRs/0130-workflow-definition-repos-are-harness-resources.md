---
title: "130. Workflow-definition repositories are harness resources"
status: Accepted
relates_to:
  - agent-architecture
  - security-threat-model
topics:
  - runtime
  - harness
  - security
---

# 130. Workflow-definition repositories are harness resources

Date: 2026-10-08

## Status

Accepted

<!-- ADRs are point-in-time records, but not fully frozen after acceptance.
     Minor annotations are welcome: cross-references to related ADRs, short
     notes linking to newer decisions, or clarifying remarks. However, do not
     substantially rewrite the Context, Decision, or Consequences sections. If
     the decision itself needs to change, write a new ADR that supersedes this
     one. For evolving design narrative, use docs/architecture.md. -->

## Context

Some teams keep a whole pipeline in one repository: skills, helper scripts,
sub-agents, and a Claude Code workflow script (`workflows/<name>.js`) that
fixes the order of the steps and fans out over work items. The repository
root is a Claude Code plugin (`.claude-plugin/plugin.json`), and Claude Code
users install it as a marketplace entry pinned to a commit sha. Running such a
*workflow-definition repository* under fullsend should keep the agent-led
path, so the runner keeps its hooks, metrics, transcripts and validation
loop; a script entrypoint above the runtime
([#7947](https://github.com/fullsend-ai/fullsend/pull/7947)) loses them.

[ADR 0094](0094-pi-extensions-are-harness-resources.md) already delivers a
Claude plugin directory, sourced like a skill
([ADR 0038](0038-universal-harness-access.md): a tree URL with a tree hash),
injection-scanned and passed to `claude` with `--plugin-dir`. A workflow that
a plugin ships runs headless as `/<plugin>:<workflow>`, also with fullsend's
`--agent <name>` on the same command line, and the result event's
`modelUsage` sums the whole tree, workflow agents included (both checked on
Claude Code 2.1.295), so the runner's metrics stay complete.

Three gaps remain. `plugins:` refuses a tree URL at the repository root, and
each harness pins each plugin separately, so every harness of a pipeline
repeats the pin. The tree fetcher refuses symlinks, and definition
repositories link skills to a shared `scripts/` directory. And nothing starts
the workflow: the agent prompt has to ask the model to call it.

## Options

- **A script entrypoint (`entrypoint:`).** Rejected: the runner sees only the
  script's exit code and stdout, so token and cost metrics, the hook chain
  and transcript checks must be rebuilt by each script, and the path is
  Claude-only.
- **Keep the sequence in the agent prompt or a skill-driven state machine.**
  Kept as supported shapes; both run on the agent-led path today and need no
  change. They do not give the pipeline author a fixed, testable order of
  steps, which is what a workflow script is for.
- **Vendor the definition repository from a host pre-script.** Works on
  released fullsend and is what consumers do today, but each consumer
  re-implements the fetch, pin check and copy, and the runner never scans
  what is vendored into the target checkout.

## Decision

A workflow-definition repository is a resource that `config.yaml` pins once
and a harness names. It is delivered through the ADR 0094 plugin path, and
the runner starts the workflow itself. Five rules govern it.

1. **Pinned once in `config.yaml`.** A top-level `workflows:` list sits
   beside `agents:`. Each entry has a `name` and a `source`:

   ```yaml
   workflows:
     - name: sample-pipeline
       source: https://github.com/example-org/sample-pipeline/tree/<commit-sha>#sha256=<tree-hash>
   ```

   A remote `source` is a forge tree URL at a full commit sha, pointing at
   the repository root or at a sub-directory, with fullsend's tree hash
   (github.com today, as for `plugins:`). These
   are the `github` (`repo` + `sha`) and `git-subdir` (`url` + `path` + `sha`)
   source shapes of a Claude Code marketplace entry, so a repository that
   already installs as a Claude Code plugin needs no new layout. The URL must
   be covered by `allowed_remote_resources`
   ([ADR 0058](0058-agent-registration.md)), and `fullsend lock` records it
   like any other dependency of each harness that names it. A local `source`
   is a path inside the git checkout that holds the `.fullsend/` directory
   (`.` is its root), so a definition repository
   can run its own pipeline without pinning itself; only files git tracks are
   read, so nothing untracked beside the checkout (another repository, a
   credentials file) is delivered. Zip archives with a digest (the
   marketplace `archive` shape) are not accepted in this decision.
2. **Fetched as exactly what is uploaded.** The fetch accepts the repository
   root and stays bounded by the tree fetcher's limits on file count and total
   size. A symlink whose target is inside the fetched tree is *materialized*:
   the target's content is stored under the link's path, and counts toward
   the limits like any other file. A symlink that leaves the tree, dangles or
   loops is refused with its path in the error. The tree that is hashed is
   therefore the tree that is injection-scanned, uploaded and preflighted,
   with no links left in it. A source path that itself passes through a
   symlink is refused, and fullsend's own configuration directory is never
   part of the tree. The directory must classify as a Claude plugin under
   `pluginformat.Detect`.
3. **The harness names it and the runner starts it.** A harness field
   `workflow:` names the definition (`definition:`, a `workflows:` name in
   `config.yaml`), the workflow inside it (`name:`, which is the script's
   `meta.name`; fullsend requires the file `workflows/<name>.js` to match),
   and optional literal `args:`. Claude Code hands the text after the command
   to the script as one string, unsplit (checked on 2.1.295). The runner uploads the definition like a `plugins:` entry and
   replaces the default prompt with `/<plugin>:<name> <args>`. `<plugin>`
   is the prefix Claude Code gives the plugin's commands: the `name` in
   `.claude-plugin/plugin.json`, or the uploaded directory's name (the
   `workflows:` entry name) when that file is absent. `--agent` stays on
   the command line, so the agent file still sets the main loop's tools and
   instructions. The run plan prints the definition, its pin and the command.
   A validation-loop retry starts the same command again; the workflow is
   expected to skip finished work from its own durable state, since a Claude
   Code workflow resumes only within the session that started it.
4. **Claude only, and refused elsewhere.** A harness with `workflow:` fails at
   plan time under any other runtime, naming the runtime and the field. The
   dummy runtime records the command it would have run, so behaviour tests can
   assert on it without a model.
5. **No new tool or hook surface.** `Workflow` is already a canonical Claude
   tool name, the tool-allowlist, canary and PostToolUse hooks already match
   every tool, and an agent with a `tools:` list must name `Workflow` for the
   main loop to start the run. Workflow agents run inside the same `claude`
   process, under the same hooks.

## Consequences

- A pipeline repository runs under fullsend with one `workflows:` entry and a
  thin harness per agent, so consumers stop vendoring it from a pre-script.
- Symlink materialization and the repository-root mode apply to this resource
  only, so the pin, scan and upload of `skills:` and `plugins:` do not change.
- An older fullsend binary silently ignores the unknown `workflows:` and
  `workflow:` keys and runs the harness with the default prompt, so consumers
  must pin the fullsend release that introduced them; the fields are additive
  and do not bump `schema_version` ([ADR
  0127](0127-harness-schema-versioning-and-field-types.md)).
- fullsend's own behaviour tests cover this path against a GitHub test
  repository; coverage of a pipeline's external systems, such as Jira, stays
  with the definition repository's integration tests.
- A workflow agent has no Agent tool (Claude Code 2.1.260, 2.1.292, 2.1.295)
  and a workflow cannot pause for a human, so a definition lifts nested
  sub-agent launches into the script and ends the run at each human gate.
- Follow-ups, each its own decision: named steps with a `resume_from` input
  started by a later `trigger:` event, a pi counterpart (a completion event
  for a script-led parent and a public child-call API), and the marketplace
  `archive` source shape.
