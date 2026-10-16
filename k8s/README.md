# Local Kubernetes dispatcher proof of concept

See [STATUS.md](STATUS.md) for the dated results and remaining work across the six milestones. [PLAN.md](PLAN.md) defines their scope and acceptance criteria.

This directory implements a local kind cluster, PostgreSQL, fixture and Jira poller profiles, dispatcher CronJobs, independent runner Jobs, the gateway execution path, an opt-in private Kubernetes-identity mint for repository-scoped GitHub App tokens, and GCP Workload Identity Federation for inference. The live profile uses real Jira discovery, project-role authorization, and CEL harness selection. Real Jira execution is disabled unless `fullsend_harnesses` explicitly opts in; the current allowlist is `triage`. A controlled stage acceptance passed on 2026-10-09, including a real triage run, Jira writeback, and verified cleanup.

## Prerequisites

Install `kind`, `kubectl`, Podman, Go, Git, OpenSSL, `jq`, ripgrep, and `sha256sum`. The scripts pin images in [kind/versions.env](kind/versions.env) and use Podman as the kind provider. They do not change your default kubeconfig or current context. Rootless kind may require host networking settings specific to the machine.

From any directory:

```sh
/path/to/fullsend/k8s/kind/deploy.sh --profile fixture
/path/to/fullsend/k8s/kind/status.sh --profile fixture
```

`deploy.sh` preserves PostgreSQL data. `reset.sh --profile fixture` deletes only the cluster named `fullsend-poc`, then rebuilds it. It requires the ownership marker and dedicated kubeconfig before deletion. Database contents are lost on reset; the ignored `k8s/.local/` inputs and `k8s/.state/` reports remain. `destroy.sh --profile fixture` deletes only the owned cluster. The baseline fixture smoke creates no remote resources; the opt-in gateway dispatch demo does.

Each numbered script under `kind/` runs separately. `06-smoke.sh` triggers the same CronJob templates manually, so a demo does not wait for a schedule. `NO_EMOJI=1` suppresses status emoji. Reports under `k8s/.state/` contain build revision, dirty tree hash, tool versions, resource summaries, and observed kind-node CPU/memory usage. They exclude Secrets and source contents.

After deploying, run this from the repository root:

```sh
k8s/kind/inject-observation.sh --profile fixture
```

The helper adds a successful observation with fresh UUID key and entity IDs to the ignored local fixture, updates its ConfigMap, and triggers the poller and dispatcher. Each invocation adds one distinct database observation and prints the runner Job name and Kubernetes UID once launched. It requires `uuidgen`. The Job is normally deleted after reconciliation; its name, UID, and outcome remain in the `attempts` table in PostgreSQL. The fixed four-observation assertion in `06-smoke.sh` will fail after injection; use `reset.sh` to return to the baseline demo.

The fixture source is [fixtures/source.json](fixtures/source.json). Its `key` is a unique observation, while `entity` can recur across revisions. [fixtures/selection.json](fixtures/selection.json) maps the baseline fixture kinds to stub harness names. The fixture poll commits observations and a checkpoint in one PostgreSQL transaction. Dispatch stores a selected run set and attempts, reserves capacity under a database advisory lock, and reconciles Jobs by deterministic name and UID. The demo limits are two global and two per tenant, with one active reservation per tenant/source/entity. Waiting, failed, and unknown work remains visible in PostgreSQL. Use `fullsend k8s-poc retry RUN_ID` or `dismiss RUN_ID` for a cleaned-up failure; unknown outcomes retain their reservation. The stub exits successfully, exits with code 23, or stays active based on its fixture behavior.

The CronJobs run every minute in UTC with `Forbid` overlap and a 50 second worker deadline. The Jobs they create outlive dispatch cycles, have no automatic TTL, and use `backoffLimit: 0`. A terminal stub Job keeps its capacity and conflict reservation until a later cycle confirms its deletion. The separate `launcher` ServiceAccount can create, read, and delete Jobs and create or read ServiceAccounts only in the runner namespace. It is trusted code: Kubernetes Job creation can select namespace ServiceAccounts, so the dispatcher uses a fixed validated Job template and untrusted agents never receive this API identity. There is no server-side admission policy in this PoC. Stub runner Pods have no Kubernetes API token. kind's default networking is not a tenant isolation boundary.

## Jira live profile

Place the following inputs under ignored `k8s/.local/live/` before deploying. Keep the token and email files private (`chmod 600`). The selected repository must have a loadable `.fullsend/config.yaml` and triggered harnesses. Use a dedicated test issue label; every current issue in that project with the label is scanned.

| Input | Content |
| --- | --- |
| `settings.json` | `{"jira_base_url":"https://YOUR-JIRA-SITE","project":"FSENDAI","label":"fullsend-dispatcher-test","repo":"ralphbean/jira-triage-test","fullsend_harnesses":[]}` |
| `repos.yaml` | A `repos.Manifest` with `version: 1` and `github.repos` containing `name: ralphbean/jira-triage-test` |
| `repo/` | Checkout of that repository at the version to use for selection |
| `jira-token` | Jira API token, with no newline requirement |
| `jira-email` | Account email associated with the token |
| `wif.json` | Required only for real Fullsend execution: `{"gcp_audience":"//iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL/providers/PROVIDER","vertex_project":"PROJECT_ID"}` |

The API identity must be able to read the project's role assignments so the poller/dispatcher can resolve whether the issue or comment author has the existing write-level authorization. Jira Cloud requires **Administer Projects** on the project (or global **Administer Jira**) for the [project roles API](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-project-roles/). The configured test account now has the project **Administer** role, and the live acceptance confirmed role lookup and actor authorization succeed. Keep the actor check enabled; do not bypass it to force dispatch.

For example, clone the repository into `k8s/.local/live/repo`, create the input files, then run:

```sh
k8s/kind/deploy.sh --profile live
k8s/kind/06-live-smoke.sh --profile live
```

`deploy.sh` validates the manifest and repository harnesses, stores a digest of the resulting snapshot with each new observation, and preserves the database. A later `deploy.sh --profile live` rebuilds from the current checkout; newly discovered activity uses that revision. Existing observations keep the revision recorded when they were discovered. The fixture and live CronJobs are mutually suspended when switching profiles, so only the selected profile runs on schedule.

The smoke script triggers poll and dispatch immediately. Its report under ignored `k8s/.state/live-smoke-*` includes control Job logs, the live observation count, and `runs.txt` with selection states, Job names, Kubernetes UIDs, and outcomes. The same evidence remains in `observations`, `selected_runs`, and `attempts` in the PostgreSQL Pod after a completed runner Job is removed. A new issue or an `/fs-triage` comment on a labeled issue should create a distinct observation if the repository harness CEL matches. A second smoke cycle reconciles a Job that was still running during the first. The test repository can contain other material: the Jira project and exact label define the source scope, and the registered checkout supplies selection context. Its existing workflows are independent of this PoC.

The poller reads comments and changelog pages for each matching issue and records an issue-specific checkpoint with its observations. Jira search returns updated issue snapshots, so this is a reconciliation poll; it cannot prove delivery of every intermediate transition. A failed search fails the cycle without changing checkpoints; a failed issue page leaves that issue's checkpoint unchanged while other issues continue. A pending candidate is refetched before selection, including current labels and the actor's project role. Removed labels and actors without a write role are denied. The PoC does not write Jira checkpoint properties. By default, live selections launch stub Jobs and do not comment on Jira.

To opt the stage test repository into the real triage run, set `fullsend_harnesses` to `["triage"]` in `settings.json`, provide the WIF audience and Vertex project in `wif.json`, deploy the private mint for the registered repository, and ensure the gateway bundle and WIF runner setup are present. Then rebuild the control image and run:

```sh
k8s/kind/02-images.sh --profile live
k8s/kind/05-live-workloads.sh --profile live
```

The live setup extends the existing WIF provider and inference service account for only the registered repository's triage ServiceAccount. It bundles the checked-out repository and its commit into an immutable ConfigMap, persists selected normalized Jira events, and sends them to the existing `fullsend run triage` command. The Jira API token is available to the trusted runner's post-run script for the expected stage comment; it is not put in the sandbox environment. The dispatcher still requires the independent cleanup Job to confirm remote absence before completing the attempt. Use one controlled question issue under the unique test label: the real triage agent can write its normal response comment to that stage issue. Do not opt ordinary project work into this path.

## HyperShell gateway setup and probe

`07-hypershell-build.sh` builds the pinned `hsctl` CLI before gateway setup, so you can use it to log in and inspect registered clusters. `07-hypershell-setup.sh` also runs the builder when `hsctl` is missing or was built from a different pinned source revision, then creates or reuses a gateway and a named gateway service account through the management API. It saves the one-time credential under ignored `k8s/.local/gateway/`, registers the gateway with OpenShell, and prints the bundle and secret paths for the probe. Exact-name checks prevent repeat runs from creating duplicates. An existing service account can be reused only if its original credential bundle is present locally; its secret cannot be retrieved from the API. The script never deletes a shared gateway or service account. It is separate from `deploy.sh`, so a kind reset never provisions or removes external resources.

The script requires the gateway to reach `Running` with a route before setting up its service account. If HyperShell reports `Degraded` or `Failed`, inspect both its `status` and provisioning conditions in the console. Conditions may all read `Complete` while the deployment status reports unavailable replicas. Restore the gateway before retrying setup or cleanup.

For this PoC's HyperShell environment, `HYPERSHELL_API_URL` is the direct management API base URL (not the browser console URL), and `HYPERSHELL_ISSUER_URL` is the Keycloak realm that performs `hsctl` device login. Use these exact values:

```sh
k8s/kind/07-hypershell-build.sh
export HYPERSHELL_API_URL='https://api.hyp2.infra.hypershell.app/'
export HYPERSHELL_ISSUER_URL='https://keycloak.hyp2.infra.hypershell.app/realms/hypershell'
export HYPERSHELL_GATEWAY_NAME='whatever'
HYPERSHELL_CONFIG=k8s/.local/gateway/hsctl-config.json k8s/.local/bin/hsctl login --no-browser --url "$HYPERSHELL_API_URL" --issuer-url "$HYPERSHELL_ISSUER_URL"
export HYPERSHELL_PLACEMENT_SELECTOR='["network/public","provider/ibm","runtime/release-check"]'
mkdir -p k8s/.local/gateway
cat > k8s/.local/gateway/request.json <<JSON
{
  "name": "$HYPERSHELL_GATEWAY_NAME",
  "placement": {"selector": $HYPERSHELL_PLACEMENT_SELECTOR},
  "route": "{\"enabled\":true}"
}
JSON
jq . k8s/.local/gateway/request.json
```

The current managed API requires a `placement.selector` of capability labels; the pinned `hsctl` CLI's create-gateway flags predate this API, but `--body` sends the JSON request through unchanged. The selector above matches all three capabilities reported for the current `hyp2-hub` and distinguishes it from the other currently registered cluster. Placement is capability-based, not an exact cluster-ID pin: the API may choose any connected cluster matching every selector label. Inspect `hsctl list managedClusters --output json` and adjust the selector if your registered clusters differ. Do not include `cluster_id` or `release_id` in a create request. Omit `--gateway-request` to reuse an existing gateway.

With console access only, the first run uses an interactive device login; `hsctl` then caches a refresh token in the ignored local config. For unattended first use, supply a private management bearer token file via `--token-file`. A gateway service account cannot authenticate to the management API. Setup defaults to `--role openshell-admin` for this dedicated PoC gateway so Jobs can access the default workspace without a separate membership step. This broad administrative access is a PoC shortcut and is not tenant-safe. An existing service account with a different role is not changed; use a new name or recreate it with the requested role. For a console-created service account whose one-time credential was saved as a raw mode-600 secret, pass `--existing-client-secret-file` and, if its OpenShell registration has a separate local name, `--openshell-name`.

```sh
k8s/kind/07-hypershell-setup.sh \
  --api-url "$HYPERSHELL_API_URL" --issuer-url "$HYPERSHELL_ISSUER_URL" \
  --gateway-name "$HYPERSHELL_GATEWAY_NAME" \
  --gateway-request k8s/.local/gateway/request.json \
  --service-account-name fullsend-poc
```

The probe requires OpenShell `0.1.2-rhaiv.0`. Its UBI10 runner layers the [digest-pinned HyperShell CLI image](runner/Containerfile) over the checkout's UBI10 runner. It copies only the selected gateway's `metadata.json` into the Pod; browser tokens, management tokens, local gateway keys, and gcloud credentials stay on the host. The sandbox image is separate from the UBI10 runner. To use the current `ghcr.io/fullsend-ai/fullsend-sandbox:latest`, resolve that tag with `skopeo inspect docker://ghcr.io/fullsend-ai/fullsend-sandbox:latest --format '{{.Digest}}'` and pass `ghcr.io/fullsend-ai/fullsend-sandbox@sha256:...` to the probe. The digest is recorded in the probe evidence.

**Major compatibility caveat:** Fullsend invokes the external `openshell` CLI; it does not embed a client version. The host preflight and the runner used by `fullsend run` must use a CLI build compatible with the provisioned HyperShell gateway. This PoC checks for `0.1.2-rhaiv.0` on the host and layers that pinned HyperShell client over the checkout-built runner. A different CLI/gateway pairing is unverified until gateway login, streaming, file transfer, and cleanup are checked together.

The current PoC Jobs reuse one gateway service account with `openshell-admin` on a dedicated test gateway. Each Job has its own sandbox name and evidence directory, but this shared administrative credential does not provide tenant-safe gateway isolation.

**Temporary TLS bypass:** The managed gateway currently serves a private-CA certificate that lacks its advertised public hostname. Until [HyperShell issue #443](https://github.com/openshift-online/hypershell/issues/443) is resolved, the host preflight and probe/cleanup runner Jobs set `OPENSHELL_GATEWAY_INSECURE=true`. This disables gateway TLS certificate verification for those processes. The scripts and Job logs print the bypass and issue link prominently. Remove it once the managed service provides a valid certificate and a supported CA distribution path.

The runner's OpenShell wrapper also prints that warning on stderr and filters the CLI's duplicate TLS warning from stdout. OpenShell otherwise mixes it into sandbox command output, which breaks JSON parsing by the pi runtime. The wrapper retains other OpenShell warnings.

```sh
k8s/kind/07-gateway-probe.sh \
  --gateway-bundle k8s/.local/gateway/openshell \
  --gateway-name "$HYPERSHELL_GATEWAY_NAME" \
  --client-secret-file "k8s/.local/gateway/${HYPERSHELL_GATEWAY_NAME}-fullsend-poc-client-secret" \
  --sandbox-image 'YOUR_SANDBOX_IMAGE@sha256:YOUR_DIGEST'
```

The probe logs into the gateway with the service account in an isolated host profile, then checks `whoami`. It builds the UBI10 runner from this checkout, loads it into the owned kind cluster, installs the connection bundle as a Secret in `fullsend-runner`, and prints the Job name. The Job logs in again with the service account before invoking Fullsend, uses the same restricted Job template as dispatch with no Kubernetes API token, and mounts a persistent evidence claim. It refreshes the short-lived service-account login before each separate gateway operation and creates sandboxes with an explicit filesystem policy that allows no network egress. It creates two remote sandboxes at once, uploads distinct files, copies each inside its sandbox, downloads the outputs, checks SHA-256 and exit status, and attempts both deletions. Job logs include a JSON result. The PVC holds `intent.json`, `result.json`, and the input/output files under `/evidence/JOB_NAME` after the Pod exits. Use the printed `kubectl logs` command to see the result; inspect the PVC before deleting the cluster if the Job was interrupted.

After the probe has installed the gateway Secrets, run `k8s/kind/09-harness-demo.sh`. It builds the current UBI10 runner, copies the pinned fixture into a ConfigMap, and launches a restricted Job that invokes `fullsend run gateway-demo`. The deterministic harness reads `sample.txt` from its target repository and writes `agent-result.json` in the remote sandbox. The Job validates the downloaded result, records its SHA-256 under `/evidence/JOB_NAME`, and deletes the sandbox. The runner refreshes its short-lived gateway service-account login before each OpenShell call. This verifies the Fullsend run path without using source or inference credentials.

### Private Kubernetes identity mint

The optional mint uses a dedicated namespace and HTTPS Service. Its trust policy contains the kind issuer, current public JWKS, an allowlist of repository IDs, installation ID, and role/level ceiling. The GitHub App PEM is supplied from an ignored local file and mounted only into the mint Pod. Projected runner tokens use the separate `fullsend-k8s-mint` audience; the runner's automatic Kubernetes API token mount remains disabled. The mint trusts only the configured issuer and public JWKS, and each GitHub installation-token request names exactly one repository ID derived from the runner identity. Before returning a token, it verifies GitHub confirmed that repository ID and exactly the requested role permissions. The trusted runner supplies `FULLSEND_MINT_URL`, `FULLSEND_REPOSITORY_ID`, `KUBERNETES_MINT_TOKEN_FILE`, and `KUBERNETES_MINT_CA_FILE`; the CLI rereads the projected token file for each exchange attempt.

Deploy it with the GitHub App ID, installation ID, one or more repository IDs, a tenant label, and a local PEM path. Every listed repository must be accessible to that installation. Repeat `--repository-id` for each repository. When listing multiple IDs, choose one for the dispatcher demo with `--dispatcher-repository-id`; that demo continues to dispatch against a single repository, which must also appear in the allowlist. For a single repository, the dispatcher ID defaults to that repository.

```bash
k8s/kind/13-deploy-private-mint.sh \
  --app-id APP_ID --installation-id INSTALLATION_ID \
  --repository-id REPOSITORY_ID_1 --repository-id REPOSITORY_ID_2 \
  --dispatcher-repository-id REPOSITORY_ID_1 --repository owner/repo \
  --private-key-file /secure/path/app.private-key.pem
```

Then run the scoped exchange and rotation probe:

```bash
k8s/kind/14-test-private-mint.sh --repository-id REPOSITORY_ID --repository owner/repo
```

The probe verifies the returned scope metadata identifies the configured repository and exact coder write permissions, including `contents:write` and `packages:read`. It waits for kubelet to rotate its 600-second projected token (up to 15 minutes), then performs a second exchange and checks the same scope again. Only the probe's generic result is saved under `k8s/.state/`; JWTs, App tokens, the App PEM, and the local mint CA are not written to logs or tracked files. The runner receives the GitHub token through a Git credential helper backed by an atomically replaced file; refresh denial removes that credential and cancels the active agent process. Runtime processes that already captured `GH_TOKEN` cannot have that environment value changed, so this refresh path keeps Git operations current while other already-running API clients may retain their original token until they restart.

JWKS verification is offline. Deleting a Pod does not revoke an already issued projected JWT immediately; it remains valid until its signed expiration (600 seconds in this probe). GitHub installation tokens have their own GitHub-supplied expiration, so deleting the runner also does not revoke a token already minted by the App. Keep both lifetimes short and treat the App permission policy as the authority for what the token can do.

To send the same fixture through the dispatcher, deploy the fixture profile with this checkout's control-plane image and run `k8s/kind/11-dispatch-gateway-demo.sh --profile fixture`. The script builds the UBI10 runner, injects one UUID-keyed `gateway_demo` observation, and adds the exact `gateway-demo` selection only to the ignored local fixture revision. The dispatcher persists the attempt before creating a Job; its sandbox name is derived from that Job name. After the runner exits, the dispatcher launches `fs-clean-<run>-<attempt>` to verify the remote sandbox is absent, then deletes the runner Job. Only confirmed Job removal after that cleanup releases capacity and entity-conflict reservations. Output and cleanup evidence remain on the PVC. `fullsend k8s-poc cancel RUN_ID` requests an active gateway-demo stop and follows the same remote cleanup gate; it finishes as a cleaned-up failure. When the optional mint has been deployed, the dispatcher ensures the configured repository's `triage` ServiceAccount and projects a 600-second mint-audience token plus the mint CA only into the trusted runner container. The script checks the live Job and ServiceAccount identity, and the normal `fullsend run` path exchanges that assertion for a repository-scoped App token. Without the mint ConfigMap, the demo keeps the existing runner identity. The fixture path uses the shared PoC gateway service account and does not pass Jira credentials to the runner.

To exercise workspace and per-run provider scoping on the dedicated test gateway, run `k8s/kind/12-workspace-isolation.sh` with the gateway name, its local service-account secret path, and a digest-pinned sandbox image:

```sh
k8s/kind/12-workspace-isolation.sh \
  --gateway-name "$HYPERSHELL_GATEWAY_NAME" \
  --client-secret-file "k8s/.local/gateway/${HYPERSHELL_GATEWAY_NAME}-fullsend-poc-client-secret" \
  --sandbox-image 'YOUR_SANDBOX_IMAGE@sha256:YOUR_DIGEST'
```

The script creates two temporary workspaces, separate run-scoped OpenAI providers with distinct random test credentials, and one pinned sandbox per workspace. It verifies that the provider and sandbox resources are visible only in their own workspace, then removes the temporary resources. The credentials are synthetic and never used for inference; only their SHA-256 fingerprints are recorded under ignored `k8s/.state/`. The runner workspaces still share the gateway service-account credential, so this demonstrates workspace resource separation rather than tenant-safe gateway authentication. If the script is interrupted, inspect the two reported workspace names and remove any remaining PoC resources before resetting kind.

Observed on 2026-10-06: managed HyperShell sandbox provisioning was intermittent. A provider-free kind demo with the pinned Fullsend image and explicit policy remained `ConfigurationPending` until a 300-second timeout; subsequent host runs on both a fresh gateway and `poc2` succeeded. A later kind demo Job (`fs-demo-62513bb9`) also succeeded: sandbox creation took 96.8 seconds, the dummy agent exited 0, output was downloaded and hashed, and sandbox deletion was verified. The precise cause of the earlier stall is unknown. Its immediate same-name retries encountered a stale workload Pod UID error, so a retry must account for remote deletion and Kubernetes reconciliation before reusing a sandbox name.

The independent WIF probe then succeeded from a UBI runner Pod: a projected `runner-inference` token was exchanged through Google STS, the scoped inference account was impersonated, and Vertex returned a Claude Haiku 4.5 response. A separate provider-bound `fullsend run` Job (`fs-demo-d4235f04`) imported the Vertex profile and reached sandbox creation, but provisioning timed out after 300 seconds while `poc2` was Degraded and its route returned TLS handshake EOF. After the service recovered, that retained sandbox was explicitly deleted and its absence verified. A later manual Job on `poc4` (`fs-demo-a7c1ea6f`) completed real inference inside the remote sandbox, validated its downloaded output, and deleted its sandbox. See [STATUS.md](STATUS.md) for the evidence and remaining acceptance gaps.

For a separate kind-to-Google inference identity, use a dedicated WIF pool/provider with the cluster's public JWKS and an exact `system:serviceaccount:fullsend-runner:runner-inference` subject binding on a scoped Vertex service account. The `runner-inference` Kubernetes service account has no automatic token mount; inference Jobs explicitly project a token with the provider audience. After every kind reset, refresh the provider's uploaded public keys before using WIF:

```sh
k8s/kind/10-refresh-inference-wif.sh \
  --project "$GCP_PROJECT_ID" --pool "$GCP_WIF_POOL" \
  --provider "$GCP_WIF_PROVIDER"
```

The script checks that the existing provider's issuer matches this cluster, rejects a JWKS containing private key fields, and writes only public keys under ignored `k8s/.local/gcp/`. It does not create IAM resources or distribute credentials. The optional pinned OpenTofu module in [`gcp/`](gcp/README.md) declares the pool, provider, inference service account and minimum IAM grants; import existing resources into local state before applying.

Run the standalone wrong-identity, Vertex and projected-token rotation checks after deploying the runner profile and refreshing WIF keys:

```sh
k8s/kind/15-wif-inference.sh \
  --project-number "$GCP_PROJECT_NUMBER" --pool "$GCP_WIF_POOL" \
  --provider "$GCP_WIF_PROVIDER" \
  --service-account-email "$GCP_INFERENCE_SERVICE_ACCOUNT" \
  --vertex-project "$GCP_PROJECT_ID"
```

Then exercise the normal Fullsend run path through HyperShell across a projected-token rotation:

```sh
k8s/kind/16-wif-fullsend.sh \
  --project-number "$GCP_PROJECT_NUMBER" --pool "$GCP_WIF_POOL" \
  --provider "$GCP_WIF_PROVIDER" \
  --service-account-email "$GCP_INFERENCE_SERVICE_ACCOUNT" \
  --vertex-project "$GCP_PROJECT_ID"
```

The second script uses an authenticated loopback refresh endpoint in the trusted runner. Fullsend refreshes the inference-only assertion into its active sandbox; the sandbox verifies the assertion changed, exchanges it, impersonates the scoped inference service account, and makes a harmless Vertex request. Its run may take about 20 minutes because the sandbox waits for a projected-token rotation and allows for a refresh tick that races with kubelet rotation. The integration evidence records assertion SHA-256 hashes and outcomes, never assertions or access tokens. Neither script uses a service-account JSON key.

A killed Pod can leave remote sandboxes named in `intent.json`. `08-gateway-cleanup.sh` stops probe, demo, and dispatcher gateway Jobs, reconciles their sandbox names through the gateway, and writes `cleanup.json` on the PVC. `destroy.sh` and `reset.sh` invoke it before deleting the cluster and stop if cleanup fails. Keep the gateway reachable until teardown finishes. The dispatcher also starts a per-attempt cleanup Job during normal reconciliation. That Job retries three transient failures; a terminal failure retains the reservation. After the gateway recovers, delete only the failed `fs-clean-<run>-<attempt>` Job and let the dispatcher recreate it. Keep the cluster and evidence PVC until remote absence is verified.

If an old gateway cannot be used for cleanup, preserve its probe evidence and explicitly retire each old Job with `08-retire-gateway-evidence.sh --old-gateway-label LABEL --job JOB --acknowledge-unverified-cleanup`. The script exports intent, result, and logs under ignored `k8s/.state/retired-gateways/LABEL/`, records the sandbox names and unverified status, and moves those directories under `retired/LABEL/` on the evidence PVC. The active cleanup scan then applies only to probes made through the replacement gateway. Retirement does not delete or prove deletion of remote sandboxes. Keep the archive after resetting kind so cleanup can be revisited if the old gateway recovers.

For a dispatcher attempt that failed before the runner wrote any evidence, whose runner Job is gone, and whose old gateway was deleted, use `08-retire-preflight-attempt.sh --attempt-id ID --old-gateway-label LABEL --acknowledge-deleted-gateway`. It requires the cleanup Job to be terminally failed, the runner Job and evidence directory to be absent, and the attempt still to hold a failed run's reservation. It archives the failed cleanup Job and logs, then atomically marks the run failed and the attempt `cleanup_state=retired`. This releases the reservation while preserving **unverified** remote cleanup status; it does not mark cleanup `done` or claim a lookup on the replacement gateway proves anything about the old one. The regular kind teardown skips these retired attempts.

## Troubleshooting

Use `kind/status.sh` to see Pods and Jobs. Check a failed control Job with the dedicated context:

```sh
kubectl --kubeconfig k8s/.local/kubeconfig --context kind-fullsend-poc -n fullsend-control logs job/JOB_NAME
```

If a rebuild fails after cluster creation, rerun `deploy.sh`. If the ownership marker or dedicated kubeconfig was lost, inspect the named cluster before restoring them; the scripts intentionally refuse to delete it automatically.

This PoC has no active-active fencing or production database operations. Remote cleanup covers only the dedicated gateway's owned PoC sandbox names. See [PLAN.md](PLAN.md) for scope and follow-up work.
