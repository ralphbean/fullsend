# Local Kubernetes dispatcher proof of concept

This directory implements milestones 1–3 of [PLAN.md](PLAN.md): a local kind cluster, PostgreSQL, fixture and Jira poller profiles, dispatcher CronJobs, and independent stub runner Jobs. The live profile uses real Jira discovery, role checks, and CEL harness selection. Milestone 4 has a manual gateway transport probe and a deterministic `fullsend run` Job with persistent output and cleanup evidence. The dispatcher still launches stub Jobs. The PoC does not use a token mint; the optional inference identity uses Google Workload Identity Federation.

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

The CronJobs run every minute in UTC with `Forbid` overlap and a 50 second worker deadline. The Jobs they create outlive dispatch cycles, have no automatic TTL, and use `backoffLimit: 0`. A terminal stub Job keeps its capacity and conflict reservation until a later cycle confirms its deletion. The launcher account can create, read, and delete Jobs only in the runner namespace. Stub runner Pods have no Kubernetes API token. kind's default networking is not a tenant isolation boundary.

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

## HyperShell gateway setup and probe

`07-hypershell-setup.sh` builds `hsctl` from a pinned HyperShell source commit, then creates or reuses a gateway and a named gateway service account through the management API. It saves the one-time credential under ignored `k8s/.local/gateway/`, registers the gateway with OpenShell, and prints the bundle and secret paths for the probe. Exact-name checks prevent repeat runs from creating duplicates. An existing service account can be reused only if its original credential bundle is present locally; its secret cannot be retrieved from the API. The script never deletes a shared gateway or service account. It is separate from `deploy.sh`, so a kind reset never provisions or removes external resources.

The script requires the gateway to reach `Running` with a route before setting up its service account. If HyperShell reports `Degraded` or `Failed`, inspect both its `status` and provisioning conditions in the console. Conditions may all read `Complete` while the deployment status reports unavailable replicas. Restore the gateway before retrying setup or cleanup.

Supply the management API URL and OIDC issuer from your HyperShell administrator. The managed console's `/api/*` route requires a browser session and does not forward a CLI bearer token, so its URL is not an `hsctl --url` substitute; use the direct management API origin. For a new gateway, place a JSON request under `k8s/.local/gateway/` with its `name`, `cluster_id`, and `release_id`, as selected in the management console. An empty `release_id` is valid for a gateway using the managed default release; include `"route":"{\"enabled\":true}"` when following the console's gateway creation request. Omit `--gateway-request` to reuse an existing gateway. With console access only, the first run uses an interactive device login; `hsctl` then caches a refresh token in the ignored local config. For unattended first use, supply a private management bearer token file via `--token-file`. A gateway service account cannot authenticate to the management API. `openshell-user` also needs membership in the intended gateway workspace; choose `--role openshell-admin` only if that broader access is intended. For a console-created service account whose one-time credential was saved as a raw mode-600 secret, pass `--existing-client-secret-file` and, if its OpenShell registration has a separate local name, `--openshell-name`.

```sh
k8s/kind/07-hypershell-setup.sh \
  --api-url "$HYPERSHELL_API_URL" --issuer-url "$HYPERSHELL_ISSUER_URL" \
  --gateway-name "$HYPERSHELL_GATEWAY_NAME" \
  --gateway-request k8s/.local/gateway/request.json \
  --service-account-name fullsend-poc
```

The probe requires OpenShell `0.1.2-rhaiv.0`. Its UBI10 runner layers the [digest-pinned HyperShell CLI image](runner/Containerfile) over the checkout's UBI10 runner. It copies only the selected gateway's `metadata.json` into the Pod; browser tokens, management tokens, local gateway keys, and gcloud credentials stay on the host. The sandbox image is separate from the UBI10 runner. To use the current `ghcr.io/fullsend-ai/fullsend-sandbox:latest`, resolve that tag with `skopeo inspect docker://ghcr.io/fullsend-ai/fullsend-sandbox:latest --format '{{.Digest}}'` and pass `ghcr.io/fullsend-ai/fullsend-sandbox@sha256:...` to the probe. The digest is recorded in the probe evidence.

The current PoC Jobs reuse one gateway service account with `openshell-admin` on a dedicated test gateway. Each Job has its own sandbox name and evidence directory, but this shared administrative credential does not provide tenant-safe gateway isolation.

**Temporary TLS bypass:** The managed gateway currently serves a private-CA certificate that lacks its advertised public hostname. Until [HyperShell issue #443](https://github.com/openshift-online/hypershell/issues/443) is resolved, the host preflight and probe/cleanup runner Jobs set `OPENSHELL_GATEWAY_INSECURE=true`. This disables gateway TLS certificate verification for those processes. The scripts and Job logs print the bypass and issue link prominently. Remove it once the managed service provides a valid certificate and a supported CA distribution path.

```sh
k8s/kind/07-gateway-probe.sh \
  --gateway-bundle k8s/.local/gateway/openshell \
  --gateway-name "$HYPERSHELL_GATEWAY_NAME" \
  --client-secret-file "k8s/.local/gateway/${HYPERSHELL_GATEWAY_NAME}-fullsend-poc-client-secret" \
  --sandbox-image 'YOUR_SANDBOX_IMAGE@sha256:YOUR_DIGEST'
```

The probe logs into the gateway with the service account in an isolated host profile, then checks `whoami`. It builds the UBI10 runner from this checkout, loads it into the owned kind cluster, installs the connection bundle as a Secret in `fullsend-runner`, and prints the Job name. The Job logs in again with the service account before invoking Fullsend, uses the same restricted Job template as dispatch with no Kubernetes API token, and mounts a persistent evidence claim. It refreshes the short-lived service-account login before each separate gateway operation and creates sandboxes with an explicit filesystem policy that allows no network egress. It creates two remote sandboxes at once, uploads distinct files, copies each inside its sandbox, downloads the outputs, checks SHA-256 and exit status, and attempts both deletions. Job logs include a JSON result. The PVC holds `intent.json`, `result.json`, and the input/output files under `/evidence/JOB_NAME` after the Pod exits. Use the printed `kubectl logs` command to see the result; inspect the PVC before deleting the cluster if the Job was interrupted.

After the probe has installed the gateway Secrets, run `k8s/kind/09-harness-demo.sh`. It builds the current UBI10 runner, copies the pinned fixture into a ConfigMap, and launches a restricted Job that invokes `fullsend run gateway-demo`. The deterministic harness reads `sample.txt` from its target repository and writes `agent-result.json` in the remote sandbox. The Job validates the downloaded result, records its SHA-256 under `/evidence/JOB_NAME`, and deletes the sandbox. The runner refreshes its short-lived gateway service-account login before each OpenShell call. This verifies the Fullsend run path without using source or inference credentials.

Observed on 2026-10-06: managed HyperShell sandbox provisioning was intermittent. A provider-free kind demo with the pinned Fullsend image and explicit policy remained `ConfigurationPending` until a 300-second timeout; subsequent host runs on both a fresh gateway and `poc2` succeeded. A later kind demo Job (`fs-demo-62513bb9`) also succeeded: sandbox creation took 96.8 seconds, the dummy agent exited 0, output was downloaded and hashed, and sandbox deletion was verified. The precise cause of the earlier stall is unknown. Its immediate same-name retries encountered a stale workload Pod UID error, so a retry must account for remote deletion and Kubernetes reconciliation before reusing a sandbox name.

The independent WIF probe then succeeded from a UBI runner Pod: a projected `runner-inference` token was exchanged through Google STS, the scoped inference account was impersonated, and Vertex returned a Claude Haiku 4.5 response. A separate provider-bound `fullsend run` Job (`fs-demo-d4235f04`) imported the Vertex profile and reached sandbox creation, but provisioning timed out after 300 seconds. At that point `poc2` was `Degraded` (`0/1 updated replicas available`) and gateway requests returned TLS handshake EOF. The failed Job and its sandbox name remain as evidence; remote deletion is unverified until the gateway responds. The WIF probe establishes kind-to-Google access, not inference inside a HyperShell sandbox.

For a separate kind-to-Google inference identity, use a dedicated WIF pool/provider with the cluster's public JWKS and an exact `system:serviceaccount:fullsend-runner:runner-inference` subject binding on a scoped Vertex service account. The `runner-inference` Kubernetes service account has no automatic token mount; a future inference Job must explicitly project a token with the provider audience. After every kind reset, refresh the provider's uploaded public keys before using WIF:

```sh
k8s/kind/10-refresh-inference-wif.sh \
  --project "$GCP_PROJECT_ID" --pool "$GCP_WIF_POOL" \
  --provider "$GCP_WIF_PROVIDER"
```

The script checks that the existing provider's issuer matches this cluster, rejects a JWKS containing private key fields, and writes only public keys under ignored `k8s/.local/gcp/`. It does not create IAM resources or distribute credentials. A scoped JSON key may be kept there for the earlier static-credential demo; WIF avoids mounting that key in Pods. Neither this identity check nor a direct Vertex request proves inference from the remote HyperShell sandbox.

A killed Pod can leave remote sandboxes named in `intent.json`. `08-gateway-cleanup.sh` stops probe and demo Jobs, reconciles their sandbox names through the gateway, and writes `cleanup.json` on the PVC. `destroy.sh` and `reset.sh` invoke it before deleting the cluster and stop if cleanup fails. Keep the gateway reachable until teardown finishes. Dispatcher Jobs still use stubs and their terminal state must not be interpreted as proof of remote sandbox cleanup.

If an old gateway cannot be used for cleanup, preserve its probe evidence and explicitly retire each old Job with `08-retire-gateway-evidence.sh --old-gateway-label LABEL --job JOB --acknowledge-unverified-cleanup`. The script exports intent, result, and logs under ignored `k8s/.state/retired-gateways/LABEL/`, records the sandbox names and unverified status, and moves those directories under `retired/LABEL/` on the evidence PVC. The active cleanup scan then applies only to probes made through the replacement gateway. Retirement does not delete or prove deletion of remote sandboxes. Keep the archive after resetting kind so cleanup can be revisited if the old gateway recovers.

## Troubleshooting

Use `kind/status.sh` to see Pods and Jobs. Check a failed control Job with the dedicated context:

```sh
kubectl --kubeconfig k8s/.local/kubeconfig --context kind-fullsend-poc -n fullsend-control logs job/JOB_NAME
```

If a rebuild fails after cluster creation, rerun `deploy.sh`. If the ownership marker or dedicated kubeconfig was lost, inspect the named cluster before restoring them; the scripts intentionally refuse to delete it automatically.

This PoC has no active-active fencing, remote cleanup for dispatcher attempts, or production database operations. The cleanup script covers only manual gateway probe resources. See [PLAN.md](PLAN.md) for later milestones and their external inputs.
