# `fullsend k8s-poc`

Local Kubernetes dispatcher proof of concept with fixture and bounded Jira profiles. See the [kind quick start](https://github.com/fullsend-ai/fullsend/blob/main/k8s/README.md) for deployment and limitations. This command group does not alter the existing GitLab or Jira `poll` paths or the GitHub event `dispatch` path.

| Command | Purpose |
| --- | --- |
| `migrate` | Apply the versioned PostgreSQL schema. |
| `poll-fixture` | Commit fixture observations and checkpoint atomically. |
| `poll-jira` | Discover issues in the configured Jira project and label, then commit observations and per-issue checkpoints. |
| `prepare-live` | Validate a registered repository and its CEL harnesses, then write a selection snapshot. |
| `dispatch` | Refetch pending Jira activity and actor roles, select harnesses with CEL, reconcile Jobs, and admit bounded runs. |
| `status` | Print durable work counts and failed run/unknown attempt IDs as JSON. |
| `gateway-probe` | From a runner Pod, create two remote sandboxes, transfer and validate files, then delete them. Requires `--name`, `--sandbox-image`, and `--output-dir`. |
| `render-gateway-probe-job` | Print a restricted Kubernetes Job using the dispatcher's Job template. Requires `--job-name`, `--probe-name`, `--runner-image`, and `--sandbox-image`. |
| `gateway-demo` | Run the pinned local `gateway-demo` harness through `fullsend run`, validate its output, and delete its sandbox. Requires `--job-name`, `--sandbox-name`, `--demo-dir`, and `--evidence-dir`. |
| `render-gateway-demo-job` | Print a restricted runner Job for the demo harness. Requires `--job-name` and `--runner-image`. |
| `cleanup-gateway-probes` | Reconcile remote sandboxes recorded by manual probes and demo runs on the evidence PVC. Requires `--evidence-dir`. |
| `render-gateway-cleanup-job` | Print a restricted cleanup Job with the gateway bundle and evidence PVC. Requires `--job-name` and `--runner-image`. |
| `retry RUN_ID` | Requeue a failed run after its Job has been cleaned up. The next dispatch makes attempt 2, 3, and so on. |
| `dismiss RUN_ID` | Acknowledge a cleaned-up failed run without rerunning it. |

`migrate`, `poll-fixture`, `poll-jira`, `dispatch`, `status`, `retry`, and `dismiss` require `DATABASE_URL`. `poll-fixture` accepts `--fixture` (default `/etc/fullsend/fixture.json`) and `--selection` (default `/etc/fullsend/selection.json`). `poll-jira` accepts `--config` (default `/etc/fullsend/live.json`), `--jira-token-file` (default `/var/run/secrets/jira/token`), and `--jira-email-file` (default `/var/run/secrets/jira/email`). `prepare-live` requires `--repos-manifest`, `--repo-dir`, `--repo`, `--jira-url`, `--jira-project`, `--jira-label`, and `--output`. The repository must be registered in the manifest and contain a loadable `.fullsend/config.yaml` with triggered harnesses. `dispatch` uses `STUB_IMAGE`, `JIRA_TOKEN_FILE`, and `JIRA_EMAIL_FILE` in the live profile, plus an in-cluster service account to create runner Jobs. The kind scripts supply these settings locally.

`gateway-probe` requires the provisioned OpenShell gateway registration in `XDG_CONFIG_HOME/openshell`, an OIDC service-account client secret, and a persistent output directory. Its image must use a SHA-256 digest. It refreshes its short-lived service-account login before each separate gateway operation and starts sandboxes with a bundled filesystem policy that allows no network egress. It writes `intent.json` before creating remote resources and writes `result.json` after transfer verification and cleanup. `render-gateway-probe-job` accepts either a runner image digest or the checkout's revision-and-content tag, and requires a digest for the sandbox image. The generated Job mounts `fullsend-gateway-bundle`, reads `fullsend-gateway-auth` as `OPENSHELL_OIDC_CLIENT_SECRET`, and mounts `fullsend-runner-evidence` in `fullsend-runner`; it does not mount Jira, forge, or app signing credentials.

`gateway-demo` invokes the normal `fullsend run gateway-demo` path with a local, digest-pinned harness and a deterministic dummy runtime. The harness reads a file from its target repository and writes JSON in the remote sandbox. The wrapper validates the downloaded JSON and SHA-256, and owns sandbox deletion even when the run fails. `render-gateway-demo-job` mounts the demo fixture ConfigMap and persistent evidence PVC. `k8s/kind/09-harness-demo.sh` builds the current UBI10 runner, installs the fixture, and starts this Job. This proves the Fullsend runner path without inference credentials; a later live agent run must separately verify provider attachment and inference.

The manual gateway probe and cleanup Jobs temporarily set `OPENSHELL_GATEWAY_INSECURE=true`, which disables gateway TLS certificate verification. The kind probe script uses the same setting for its host preflight. The host preflight and Jobs run `openshell gateway login` with the service-account secret before invoking the gateway. Each invocation logs a warning and links to [HyperShell issue #443](https://github.com/openshift-online/hypershell/issues/443), which tracks the managed gateway CA and public-hostname certificate problem.
