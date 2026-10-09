# Run a workflow definition repository

A **workflow definition** is a Claude Code plugin that ships a workflow script
(`workflows/<name>.js`) that fixes the order of a pipeline's steps. This guide
shows you how to run one under fullsend: you pin the definition once in
`.fullsend/config.yaml`, scaffold an agent that names it, and the runner starts
the workflow for you. To make your own pipeline repository loadable, see
[Make your pipeline repository loadable by fullsend](workflow-definition-authors.md).
The design is recorded in
[ADR 0130](../../ADRs/0130-workflow-definition-repos-are-harness-resources.md).

The examples use a sample definition, `sample-pipeline`, kept at
`pipelines/sample-pipeline/` in a repository checked out at `~/src/sample-repo`.
Its workflow `triage-fanout` reads an issue, checks each unchecked checklist
item in parallel, and writes a result. The same files ship in fullsend at
[`e2e/behaviour/fixtures/workflow/sample-pipeline/`](https://github.com/fullsend-ai/fullsend/tree/main/e2e/behaviour/fixtures/workflow/sample-pipeline).

## Pick a route and a source

You choose two things: where the run happens, and where the definition comes
from. Every pairing works.

| Route | Who starts the run | How to tell it applies |
|---|---|---|
| **Local run** | You, with `fullsend run` on your machine, against an OpenShell gateway | You are testing before you commit, or debugging a run |
| **CI run** | A slash command on an issue or pull request, through the per-repo install's dispatch workflow | The repository has `.github/workflows/fullsend.yaml` from `fullsend github setup` |

| Source | Use it when | What you write in `source:` |
|---|---|---|
| **Local** | The definition lives in the same repository as `.fullsend/` | A path from the top of the checkout, such as `pipelines/sample-pipeline` |
| **Remote** | The definition lives in another GitHub repository | A github.com tree URL at a full commit sha, with `#sha256=<tree hash>` |

Steps 1 to 4 are the same for both routes. Step 5 is the local route, step 7
the CI route.

## Prerequisites

- **A fullsend binary that knows `workflows:`.** Check it:

  ```console
  $ fullsend agent new --help | grep -- --workflow
        --workflow string       start a workflow: <definition>/<name>, a config.yaml workflows: name and the workflow in it (claude runtime only)
  ```

  No output means your binary predates workflow definitions. An older binary
  **silently ignores** the `workflows:` and `workflow:` keys and runs the
  agent's default prompt, so check this before you trust a run. For the CI
  route the same rule applies to the fullsend release your
  `.github/workflows/fullsend.yaml` pins (the tag after `reusable-dispatch.yml@<sha> #`).
- **The `claude` runtime.** Any other runtime refuses a harness with
  `workflow:` before the sandbox starts, because only Claude Code runs
  workflow scripts.
- **For the local route:** OpenShell with a running gateway, Google Cloud
  credentials and a GitHub token. Follow
  [Running agents locally](running-agents-locally.md#prerequisites) to set
  them up.
- **For the CI route:** a per-repo install from
  [Configuring GitHub](../getting-started/configuring-github.md).
- **For a remote source:** the definition pushed to github.com. Tree URLs on
  other forges are refused today; a local source works on any forge.

## Step 1: Pin the definition

Add one `workflows:` entry. Every harness that runs the definition names this
entry, so you pin it once.

### Local source

The sample repository pins its own `pipelines/sample-pipeline` directory:

```yaml
# .fullsend/config.yaml
version: "1"
workflows:
  - name: sample-pipeline
    source: pipelines/sample-pipeline
```

The runner reads a local source from the git index: only files git tracks are
delivered. Check what it will deliver:

```console
$ git ls-files pipelines/sample-pipeline
pipelines/sample-pipeline/.claude-plugin/plugin.json
pipelines/sample-pipeline/scripts/checklist.sh
pipelines/sample-pipeline/skills/read-issue/SKILL.md
pipelines/sample-pipeline/workflows/triage-fanout.js
```

Rules for a local source, each with its reason:

- **Commit or `git add` every file.** An untracked file is left out without an
  error, so nothing untracked beside the checkout (a credentials file, a
  nested repository) can reach the sandbox.
- **Keep it outside `.fullsend/`.** The configuration directory is never part
  of a definition, so a source inside it is refused. When `.fullsend/` lies
  under the source (for example `source: .`), it is left out.
- **Run from a git checkout.** fullsend reads the index of the repository that
  holds `.fullsend/`.

### Remote source

Pin another repository's definition to a full commit sha and its tree hash,
and allow the URL prefix. The example pins the sample from the public
fullsend repository, where it lives at
`e2e/behaviour/fixtures/workflow/sample-pipeline`. The `fullsend lock` steps
need the agent from [step 2](#step-2-scaffold-the-agent), so scaffold it
first if you have not. The later steps use the local source; with a remote
pin, the run's `Workflow:` line names the commit instead of the local path.

1. **Get the commit sha.** In a clone of the definition repository, at the
   commit you pushed:

   ```console
   $ git rev-parse HEAD
   38a221b62e7fbe7589c4dee4b02bec68b1b62ead
   ```

   Without a clone, `git ls-remote <repository URL> <branch>` prints the sha
   of a pushed branch head.

   Use the full 40-character sha. A branch or tag is refused, so the
   definition cannot change under the pin. With `main` in place of the sha,
   `fullsend lock` stops with:

   ```text
   Error: resolving workflow definition: config.yaml workflows[0] "sample-pipeline": source ref "main" is not a commit sha; pin the commit sha (40 hex characters) so the definition cannot move under the pin
   ```

2. **Pin 64 zeros as the tree hash.** No command prints the tree hash on its
   own; `fullsend lock` computes it and reports it when the pin is wrong:

   ```yaml
   # .fullsend/config.yaml
   workflows:
     - name: sample-pipeline
       source: https://github.com/fullsend-ai/fullsend/tree/38a221b62e7fbe7589c4dee4b02bec68b1b62ead/e2e/behaviour/fixtures/workflow/sample-pipeline#sha256=0000000000000000000000000000000000000000000000000000000000000000
   ```

3. **Allow the URL prefix.** The default `allowed_remote_resources` covers
   only raw.githubusercontent.com prefixes for fullsend's own repositories,
   not tree URLs on github.com, so the first lock stops and names the prefix
   to add:

   ```console
   $ fullsend lock sample-triage
   ⚡ fullsend dev
     Autonomous agentic development for Git-hosted organizations
   → Locking dependencies: sample-triage

     ✗ Workflow definition resolution failed
   Error: resolving workflow definition: config.yaml workflows[0] "sample-pipeline": source URL "https://github.com/fullsend-ai/fullsend/tree/38a221b62e7fbe7589c4dee4b02bec68b1b62ead/e2e/behaviour/fixtures/workflow/sample-pipeline" is not covered by allowed_remote_resources; add a prefix such as https://github.com/fullsend-ai/fullsend/ to allowed_remote_resources in config.yaml
   ```

   Add it to `.fullsend/config.yaml`:

   ```yaml
   allowed_remote_resources:
     - https://github.com/fullsend-ai/fullsend/
   ```

4. **Read the tree hash from the mismatch.** Lock again. The error names the
   real hash after `the fetched tree hashes to sha256=`:

   ```console
   $ fullsend lock sample-triage
   ⚡ fullsend dev
     Autonomous agentic development for Git-hosted organizations
   → Locking dependencies: sample-triage

     ✗ Workflow definition resolution failed
   Error: resolving workflow definition: workflows[0] "sample-pipeline": tree hash mismatch for https://github.com/fullsend-ai/fullsend/tree/38a221b62e7fbe7589c4dee4b02bec68b1b62ead/e2e/behaviour/fixtures/workflow/sample-pipeline: config.yaml pins sha256=0000000000000000000000000000000000000000000000000000000000000000 but the fetched tree hashes to sha256=e645c8d64206ff44d00ead5cd3f1ac5002b300771bc9f07a494873fdcf864cb7; if the commit is the one you meant, update the #sha256= fragment
   ```

5. **Copy the hash into the fragment and lock.**

   ```console
   $ grep "source: https" .fullsend/config.yaml
       source: https://github.com/fullsend-ai/fullsend/tree/38a221b62e7fbe7589c4dee4b02bec68b1b62ead/e2e/behaviour/fixtures/workflow/sample-pipeline#sha256=e645c8d64206ff44d00ead5cd3f1ac5002b300771bc9f07a494873fdcf864cb7
   $ fullsend lock sample-triage
   ⚡ fullsend dev
     Autonomous agentic development for Git-hosted organizations
   → Locking dependencies: sample-triage

     • Writing lock file
     ✓ Locked 1 dependencies for sample-triage -> ~/src/sample-repo/.fullsend/lock.yaml
         workflow: https://github.com/fullsend-ai/fullsend/tree/38a221b62e7fbe7589c4dee4b02bec68b1b62ead/e2e/behaviour/fixtures/workflow/sample-pipeline (fetched)
   ```

   `.fullsend/lock.yaml` (format `version: 2`) now records the definition as a
   `workflow` dependency, with the hash of each file (`...` marks cut lines):

   ```console
   $ cat .fullsend/lock.yaml
   # Generated by fullsend lock — DO NOT EDIT
   version: 2
   ...
   harnesses:
       sample-triage:
           source: harness/sample-triage.yaml
   ...
           dependencies:
               - field: workflow
                 url: https://github.com/fullsend-ai/fullsend/tree/38a221b62e7fbe7589c4dee4b02bec68b1b62ead/e2e/behaviour/fixtures/workflow/sample-pipeline
                 sha256: e645c8d64206ff44d00ead5cd3f1ac5002b300771bc9f07a494873fdcf864cb7
                 type: directory
                 files:
                   - path: .claude-plugin/plugin.json
   ...
                   - path: workflows/triage-fanout.js
   ...
   ```

   Commit `lock.yaml` with `config.yaml`. A run warns when the two disagree
   (see [Troubleshooting](#troubleshooting)).

This is the hash a local run of the same files reports: the local run in
[step 5](#step-5-run-locally) records the same
`e645c8d64206ff44d00ead5cd3f1ac5002b300771bc9f07a494873fdcf864cb7` as
`pin_sha256` in `metrics.json` (see [step 6](#step-6-read-the-run-metrics)).
The two match when the definition's tracked files match that commit and
`.fullsend/` is not under the source.

A local source has no pin and no lock entry.

## Step 2: Scaffold the agent

```console
$ fullsend agent new sample-triage --workflow sample-pipeline/triage-fanout --description "Triage an issue with the sample pipeline"
  ✓ Created agent "sample-triage" in .fullsend
  harness/sample-triage.yaml
  agents/sample-triage.md
  schemas/sample-triage-result.schema.json
  scripts/post-sample-triage.sh
  policies/base.yaml
  ✓ Added agent "sample-triage"
    The harness starts workflow triage-fanout from definition "sample-pipeline", which config.yaml pins to pipelines/sample-pipeline

Next:
  1. Fill in the marked sections of agents/sample-triage.md — the runner starts the workflow; that file sets the main loop's instructions and tools.
  2. Test locally, printing the result instead of commenting:
       POST_SAMPLE_TRIAGE_DRY_RUN=1 fullsend run sample-triage \
         --target-repo . --env-file .env.local
     .env.local needs GITHUB_ISSUE_URL, ISSUE_NUMBER, REPO_FULL_NAME,
     GH_TOKEN, ANTHROPIC_VERTEX_PROJECT_ID, CLOUD_ML_REGION, and
     GOOGLE_APPLICATION_CREDENTIALS pointing at a GCP credentials file;
     the run stops before it starts without them.
     GH_TOKEN must be a real token: a connectivity check runs before the
     agent does. See docs/guides/user/running-agents-locally.md.
  3. Commit .fullsend, then comment `/fs-sample-triage` on an issue or pull request to run it in CI.
```

`--workflow <definition>/<name>` takes the `workflows:` entry name and the
workflow's `meta.name`. It writes two things that matter here:

```console
$ grep -A2 "^workflow:" .fullsend/harness/sample-triage.yaml
workflow:
  definition: sample-pipeline
  name: triage-fanout
$ grep "^tools:" .fullsend/agents/sample-triage.md
tools: Bash(gh,jq), Read, Grep, Glob, Write, Workflow
```

Keep `Workflow` in `tools:`: the runner refuses an agent whose `tools:` list
leaves it out, because the main loop must call the Workflow tool to start the
run. When `config.yaml` has no entry of that name yet, `agent new` prints a
`! The harness starts workflow ...` warning with an example entry instead; it
never writes `workflows:` for you. Flag details:
[`agent new` § Starting a workflow](../../cli/agent.md#starting-a-workflow).

Fill in the marked sections of `agents/sample-triage.md`. The runner passes the
workflow command instead of a prompt, and the agent file still sets the main
loop's instructions and tools.

## Step 3: Pass args to the workflow

Add `args:` under `workflow:` in the harness. `${VAR}` references expand from
the runner environment, so the harness can name the work item:

```console
$ grep -A3 "^workflow:" .fullsend/harness/sample-triage.yaml
workflow:
  definition: sample-pipeline
  name: triage-fanout
  args: issue ${ISSUE_NUMBER}
```

The script receives the expanded text as one string in `args`, with no
splitting or quoting. It must stay on one line: a variable that holds a
newline, carriage return or NUL fails the run before the sandbox starts.

Name work-item identifiers only. Args reach the model prompt, the run plan
and `metrics.json`, so a variable whose name looks like a credential (ending
in `_TOKEN`, `_API_KEY`, `_PRIVATE_KEY`, `_ACCESS_KEY`, `_SECRET_KEY`, `_PROXY`, `_PASSWORD` or `_CREDENTIALS`,
containing `_SECRET`, or starting with `OTEL_`) is refused when the harness
loads, and an expanded value that looks like a credential fails the run.

## Step 4: Check before you run

Commit `.fullsend/` and the definition, then check the definition with
fullsend's own resolver. `fullsend lock` runs the same definition checks as
`fullsend run` without a gateway:

```console
$ fullsend lock --all
⚡ fullsend dev
  Autonomous agentic development for Git-hosted organizations
→ Locking all harnesses

→ Locking dependencies: sample-triage

  ✓ Harness has no remote dependencies — nothing to lock
  ✓ No harnesses have remote dependencies — nothing to lock
```

A local source has nothing to lock, so success is the line
`✓ Harness has no remote dependencies — nothing to lock`. With the remote
pin locked, `fullsend lock sample-triage` prints
`✓ Lock entry for sample-triage is up to date (1 dependencies)`. Any definition
problem prints `✗ Workflow definition resolution failed` and an `Error:` line
from the [troubleshooting table](#troubleshooting). `fullsend lock` also
refuses a credential-shaped variable in `args`. Three checks need
`fullsend run`: the runtime, the agent's `tools:`, and the expanded `args`.
`fullsend run` runs them before it checks for OpenShell, so a failure costs
no sandbox.

## Step 5: Run locally

```bash
POST_SAMPLE_TRIAGE_DRY_RUN=1 fullsend run sample-triage --target-repo . --env-file .env.local --output-dir ../out
```

`.env.local` needs `GITHUB_ISSUE_URL`, `ISSUE_NUMBER`, `REPO_FULL_NAME`,
`GH_TOKEN`, `ANTHROPIC_VERTEX_PROJECT_ID`, `CLOUD_ML_REGION` and
`GOOGLE_APPLICATION_CREDENTIALS`. This run pointed them at the public issue
[fullsend-ai/fullsend#8204](https://github.com/fullsend-ai/fullsend/issues/8204).
`POST_SAMPLE_TRIAGE_DRY_RUN=1` makes the post-script print the comment instead
of posting it. Output, trimmed to the lines that matter (`…` marks cut lines):

```text
→ Running agent: sample-triage

  ✓ Agent sample-triage resolved from config (local path)
…
  ✓ Harness loaded (0.0s)
…
    Runtime: claude (from ~/src/sample-repo/.fullsend/config.yaml)
…
    Workflow: sample-pipeline (local pipelines/sample-pipeline, sha256:e645c8d64206) → /sample-pipeline:triage-fanout issue 8204
…
  • Creating sandbox: fs-sam-c5e4e1a34faa
  ✓ Sandbox created (5.7s)
…
  • Running agent

…
→ Agent: claude-opus-4-6 (v2.1.258)
  🧠 The user wants me to run the "sample-pipeline:triage-fanout" workflow with the args "issue 8204". Let me invoke it.
  ⚙ Workflow
…
  💬 The workflow completed successfully with 4 agents in ~56 seconds. Let me write the result file.
…
  ✓ Agent exited with code 0 (106.4s)
…
    Run directory: ~/src/out/fs-sam-c5e4e1a34faa
…
post-sample-triage: dry run, not posting
  ✓ Post-script completed (0.3s)
```

What success looks like:

- **Before the sandbox:** the plan block prints a `Workflow:` line. It names
  the definition, its source (`local <path>`, or the first 12 characters of
  the commit for a remote pin), the first 12 characters of the tree hash, and
  the command the runner starts, `/<plugin name>:<workflow> <args>`. No
  `Workflow:` line means the harness has no `workflow:` or the binary ignores
  it (see [Prerequisites](#prerequisites)). Line format:
  [`fullsend run` § Workflow line](../../cli/run.md#workflow-line).
- **In the agent stream:** a `⚙ Workflow` tool call.
- **At the end:** the agent exits with code 0 and the post-script runs.

The run directory is under `--output-dir`, here `../out`, next to the
repository (without the flag: `fullsend` in your system's temporary
directory).

## Step 6: Read the run metrics

The runner records what it started in the run directory's `metrics.json`:

```console
$ jq .workflow ~/src/out/fs-sam-c5e4e1a34faa/metrics.json
{
  "definition": "sample-pipeline",
  "source": "pipelines/sample-pipeline",
  "pin_sha256": "e645c8d64206ff44d00ead5cd3f1ac5002b300771bc9f07a494873fdcf864cb7",
  "command": "/sample-pipeline:triage-fanout issue 8204"
}
```

| Field | Meaning |
|---|---|
| `definition` | The `workflows:` entry name |
| `source` | The local path, or the tree URL without `#sha256=` |
| `pin_sha256` | The full tree hash of what was uploaded |
| `command` | The command the runner started, with `args` expanded |

Token and cost totals in the same file include the workflow's agents. Field
reference: [`fullsend run` § metrics.json fields](../../cli/run.md#metricsjson-fields).

## Step 7: Run in CI

1. Commit `.fullsend/` (and a local definition) and merge it to the default
   branch.
2. Comment `/fs-sample-triage` on an issue.

Not run here: needs a pushed commit and a per-repo install.

CI reads a local source from the commit the dispatch workflow checks out: the
pull request's base commit for pull request events, otherwise the commit that
triggered the run (the default branch for an issue comment). Merge a
definition change before you expect CI to use it. A remote source runs
exactly the pinned commit on both routes. The run's `metrics.json` is in the
workflow run's `fullsend-sample-triage` artifact, with the same `workflow` object as step 6.

## Troubleshooting

`fullsend lock` and `fullsend run` print each error after `Error: `. Errors
from the definition checks also start with
`resolving workflow definition: `, after the step line
`✗ Workflow definition resolution failed`. The runtime and `tools:` errors
are also the text of their `✗` step line. "Seen with" names the command that
printed the text for this page. `fullsend run` prints every `run` row before
it checks for OpenShell or creates a sandbox.

| Error text | Cause and fix | Seen with |
|---|---|---|
| `workflow: needs the claude runtime; agent "<agent>" resolves to "<runtime>"; set runtime: claude for this agent or remove workflow: from its harness` | The agent resolves to pi or codex. Set `runtime: claude` on the agent's `agents:` entry, or drop the `--runtime`/`FULLSEND_RUNTIME` override. Printed for `fullsend run sample-triage --runtime pi`. | `run` |
| `workflow: agent <agent> lists tools: without Workflow, so it cannot start /<plugin>:<workflow>; add Workflow to its tools:` | The agent file's `tools:` lacks `Workflow`. Add it. | `run` |
| `workflow: agent <agent> lists Workflow in disallowedTools:, so it cannot start /<plugin>:<workflow>; remove Workflow from its disallowedTools:` | The agent file blocks the tool. Remove it from `disallowedTools:`. | `run` |
| `workflow.args "<args>" expands to more than one line; the variables it names must not hold NUL, carriage return or newline characters` | A variable in `args` holds a line break. Name a single-line variable, such as an issue number. Printed after `✗ Environment validation failed`; the error quotes `args` before expansion, here `issue ${ISSUE_NUMBER}`. | `run` |
| `workflow.args references ${<NAME>}, which names a credential (<rule>); pass work-item identifiers such as ${ISSUE_NUMBER} instead` | `args` names a credential-shaped variable, such as `${GH_TOKEN}`. Name the work item instead. `fullsend lock` prints it after `✗ Failed to load harness`. | `lock` |
| `workflow.args expands to a value that looks like a credential; pass work-item identifiers only` | A variable in `args` holds a token-like value. Name a variable that holds the work item. Printed after `✗ Environment validation failed`. | `run` |
| `workflows "<definition>": workflows/<workflow>.js declares meta.name "<meta-name>"; Claude Code runs it as /<plugin>:<meta-name>, so rename the file or set meta.name to "<workflow>"` | File name and `meta.name` differ. Make them identical. | `lock` |
| `` workflows "<definition>": workflows/<workflow>.js: the script does not start with `export const meta = {`; Claude Code lists a workflow only when `export const meta = { ... }` is the script's first statement and a plain object literal with a quoted name, and fullsend needs `;` right after its closing brace `` | Code comes before the meta object. Move `export const meta = { ... };` to the top; comments may precede it. | `lock` |
| `` workflows "<definition>": workflows/<workflow>.js: end the meta statement with `;` directly after its closing brace; Claude Code lists a workflow only when `export const meta = { ... }` is the script's first statement and a plain object literal with a quoted name, and fullsend needs `;` right after its closing brace `` | The meta object ends in `}` without `;`. Write `};`. | `lock` |
| `workflows "<definition>": has no workflows/<workflow>.js; the definition ships: <names>` | The harness `workflow.name` matches no tracked script. Fix the name, or `git add` the script: an untracked file is not delivered. | `lock` |
| `workflow.definition "<definition>" is not a config.yaml workflows: name (available: <names>); add it under workflows: or use one of those names` | The harness names an entry `config.yaml` lacks. | `lock` |
| `workflow.definition "<definition>": config.yaml has no workflows: entries; pin the definition there under workflows: with a name and a source` | Add the [`workflows:` entry](#step-1-pin-the-definition). | `lock` |
| `workflows[<i>] "<definition>": local source "<source>": <path> is a symlink to <target>, which leaves the source directory; point the link inside the repository or commit the file` | A link points outside the definition. Link inside it, or commit a copy. A remote source says `the fetched tree` instead of `the source directory`; an absolute target adds `is an absolute path, so it`. | `lock` |
| `workflows "<definition>": <source> is not a Claude Code plugin: add .claude-plugin/plugin.json at the definition root` | The source has no plugin manifest at its root. Point `source:` at the plugin root, or add the manifest. | `lock` |
| `workflows[<i>] "<definition>": source "<source>" is inside fullsend's configuration directory .fullsend/, which is never part of a definition; put the definition in another directory and set source to its path` | Move the definition out of `.fullsend/`. | `lock` |
| `workflows[<i>] "<definition>": a local workflows: source needs the repository that holds .fullsend/ to be a git checkout, and <dir> is not inside one (<git error>); run from a git checkout or pin a remote source` | Run from a clone, not an exported copy. | `lock` |
| `` workflows[<i>] "<definition>": local source "<source>": "<source>" holds no files git tracks; commit or `git add` the definition's files `` | Nothing under the source is tracked, for example a new directory you have not added yet. Commit the definition. | `lock` |
| `config.yaml workflows[<i>] "<definition>": source URL "<url>" is not covered by allowed_remote_resources; add a prefix such as https://github.com/<owner>/<repo>/ to allowed_remote_resources in config.yaml` | The default allowlist does not cover github.com tree URLs. Add the suggested prefix, as in [Remote source](#remote-source). | `lock` |
| `config.yaml workflows[<i>] "<definition>": source ref "<ref>" is not a commit sha; pin the commit sha (40 hex characters) so the definition cannot move under the pin` | The tree URL names a branch or tag. Pin the full commit sha. | `lock` |
| `workflows[<i>] "<definition>": tree hash mismatch for <url>: config.yaml pins sha256=<pinned> but the fetched tree hashes to sha256=<actual>; if the commit is the one you meant, update the #sha256= fragment` | The pin does not match the commit's tree. If the commit is right, copy `<actual>` into the fragment. | `lock` |
| `Workflow definition differs from lock.yaml: config.yaml pins workflows "<definition>" to <url>#sha256=<hash>, but lock.yaml has <url>#sha256=<hash>; this run uses the config.yaml pin. Run 'fullsend lock <agent>' to update.` | A warning (`!`) printed after the definition resolves: you changed the pin but not the lock. The run goes on with the `config.yaml` pin. Run the suggested command. | `run` |
| `lock.yaml has no entry for <agent>, so workflows "<definition>" (<url>) is not locked. Run 'fullsend lock <agent>' to update.` | A warning (`!`) printed only when `lock.yaml` exists but has no entry for this agent, for example a second agent added after the first lock. Lock the agent once. | `run` |

The run checks every `workflows:` entry on every run, also for agents without
`workflow:`, so a broken entry stops all agents in the repository. Full field
rules: [`config.yaml` `workflows:`](../../reference/config-reference.md#workflows)
and [harness `workflow`](../../reference/harness-reference.md#field-details).

## See also

- [Make your pipeline repository loadable by fullsend](workflow-definition-authors.md) — the author's side
- [Running agents locally](running-agents-locally.md) — local run setup and lock behaviour
- [Bring Your Own Agent](bring-your-own-agent.md) — what `fullsend agent new` generates
- [ADR 0130](../../ADRs/0130-workflow-definition-repos-are-harness-resources.md) — why definitions are pinned in `config.yaml`
