# Local Kubernetes dispatcher proof of concept

This directory implements milestones 1–3 of [PLAN.md](PLAN.md): a local kind cluster, PostgreSQL, fixture and Jira poller profiles, dispatcher CronJobs, and independent stub runner Jobs. The live profile uses real Jira discovery, role checks, and CEL harness selection. It does not run agents through HyperShell or use a token mint or cloud credentials.

## Prerequisites

Install `kind`, `kubectl`, Podman, Go, Git, OpenSSL, `jq`, and `sha256sum`. The scripts pin images in [kind/versions.env](kind/versions.env) and use Podman as the kind provider. They do not change your default kubeconfig or current context. Rootless kind may require host networking settings specific to the machine.

From any directory:

```sh
/path/to/fullsend/k8s/kind/deploy.sh --profile fixture
/path/to/fullsend/k8s/kind/status.sh --profile fixture
```

`deploy.sh` preserves PostgreSQL data. `reset.sh --profile fixture` deletes only the cluster named `fullsend-poc`, then rebuilds it. It requires the ownership marker and dedicated kubeconfig before deletion. Database contents are lost on reset; the ignored `k8s/.local/` inputs and `k8s/.state/` reports remain. `destroy.sh --profile fixture` deletes only the owned cluster. The fixture profile creates no remote resources.

Each numbered script under `kind/` runs separately. `06-smoke.sh` triggers the same CronJob templates manually, so a demo does not wait for a schedule. `NO_EMOJI=1` suppresses status emoji. Reports under `k8s/.state/` contain build revision, dirty tree hash, tool versions, resource summaries, and observed kind-node CPU/memory usage. They exclude Secrets and source contents.

After deploying, run this from the repository root:

```sh
k8s/kind/inject-observation.sh --profile fixture
```

The helper adds a successful observation with fresh UUID key and entity IDs to the ignored local fixture, updates its ConfigMap, and triggers the poller and dispatcher. Each invocation adds one distinct database observation and prints the runner Job name and Kubernetes UID once launched. It requires `uuidgen`. The Job is normally deleted after reconciliation; its name, UID, and outcome remain in the `attempts` table in PostgreSQL. The fixed four-observation assertion in `06-smoke.sh` will fail after injection; use `reset.sh` to return to the baseline demo.

The fixture source is [fixtures/source.json](fixtures/source.json). Its `key` is a unique observation, while `entity` can recur across revisions. [fixtures/selection.json](fixtures/selection.json) maps fixture kinds to stub harness names. The fixture poll commits observations and a checkpoint in one PostgreSQL transaction. Dispatch stores a selected run set and attempts, reserves capacity under a database advisory lock, and reconciles Jobs by deterministic name and UID. The demo limits are two global and two per tenant, with one active reservation per tenant/source/entity. Waiting, failed, and unknown work remains visible in PostgreSQL. Use `fullsend k8s-poc retry RUN_ID` or `dismiss RUN_ID` for a cleaned-up failure; unknown outcomes retain their reservation. The stub exits successfully, exits with code 23, or stays active based on its fixture behavior.

The CronJobs run every minute in UTC with `Forbid` overlap and a 50 second worker deadline. The Jobs they create outlive dispatch cycles, have no automatic TTL, and use `backoffLimit: 0`. The launcher account can create, read, and delete Jobs only in the runner namespace. Stub runner Pods have no Kubernetes API token. kind's default networking is not a tenant isolation boundary.

## Jira live profile

Place the following inputs under ignored `k8s/.local/live/` before deploying. Keep the token and email files private (`chmod 600`). The selected repository must have a loadable `.fullsend/config.yaml` and triggered harnesses. Use a dedicated test issue label; every current issue in that project with the label is scanned.

| Input | Content |
| --- | --- |
| `settings.json` | `{"jira_base_url":"https://YOUR-JIRA-SITE","project":"FSENDAI","label":"fullsend-dispatcher-test","repo":"ralphbean/jira-triage-test"}` |
| `repos.yaml` | A `repos.Manifest` with `version: 1` and `github.repos` containing `name: ralphbean/jira-triage-test` |
| `repo/` | Checkout of that repository at the version to use for selection |
| `jira-token` | Jira API token, with no newline requirement |
| `jira-email` | Account email associated with the token |

For example, clone the repository into `k8s/.local/live/repo`, create the input files, then run:

```sh
k8s/kind/deploy.sh --profile live
k8s/kind/06-live-smoke.sh --profile live
```

`deploy.sh` validates the manifest and repository harnesses, stores a digest of the resulting snapshot with each new observation, and preserves the database. A later `deploy.sh --profile live` rebuilds from the current checkout; newly discovered activity uses that revision. Existing observations keep the revision recorded when they were discovered. The fixture and live CronJobs are mutually suspended when switching profiles, so only the selected profile runs on schedule.

The smoke script triggers poll and dispatch immediately. Its report under ignored `k8s/.state/live-smoke-*` includes control Job logs, the live observation count, and `runs.txt` with selection states, Job names, Kubernetes UIDs, and outcomes. The same evidence remains in `observations`, `selected_runs`, and `attempts` in the PostgreSQL Pod after a completed runner Job is removed. A new issue or an `/fs-triage` comment on a labeled issue should create a distinct observation if the repository harness CEL matches. A second smoke cycle reconciles a Job that was still running during the first. The test repository can contain other material: the Jira project and exact label define the source scope, and the registered checkout supplies selection context. Its existing workflows are independent of this PoC.

The poller reads comments and changelog pages for each matching issue and records an issue-specific checkpoint with its observations. Jira search returns updated issue snapshots, so this is a reconciliation poll; it cannot prove delivery of every intermediate transition. A failed search fails the cycle without changing checkpoints; a failed issue page leaves that issue's checkpoint unchanged while other issues continue. A pending candidate is refetched before selection, including current labels and the actor's project role. Removed labels and actors without a write role are denied. The PoC does not write Jira checkpoint properties. The live profile still launches only stub Jobs; it does not comment on or modify Jira issues.

## Troubleshooting

Use `kind/status.sh` to see Pods and Jobs. Check a failed control Job with the dedicated context:

```sh
kubectl --kubeconfig k8s/.local/kubeconfig --context kind-fullsend-poc -n fullsend-control logs job/JOB_NAME
```

If a rebuild fails after cluster creation, rerun `deploy.sh`. If the ownership marker or dedicated kubeconfig was lost, inspect the named cluster before restoring them; the scripts intentionally refuse to delete it automatically.

This PoC has no deliberate active-active fencing, remote sandbox cleanup, or production database operations. See [PLAN.md](PLAN.md) for later milestones and their external inputs.
