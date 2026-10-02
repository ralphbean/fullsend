# Local dispatcher and poller PoC on kind

Planning handoff, 2026-10-02. Implementation has not started. Baseline checkout: `90833a878`.

Build this incrementally in `fullsend-ai/fullsend`. The first useful stopping point is milestone 2: a reproducible, credential-free poll → PostgreSQL → dispatch → Kubernetes Job → recorded result demonstration. Milestone 4 adds representative Fullsend work through a user-provisioned HyperShell gateway. Milestones 5 and 6 independently replace repository and inference credentials with workload identity.

## Inputs and decisions

- Primary design: `/home/rbean/code/notes/dispatch-architecture-github-gitlab-kubernetes.html`, especially the invocation, concurrency, identity, and recovery sections.
- Background: the backlog snapshot, document update/revision plans, historical problems review, and four `follow-ups/` proposals in that directory. The current HTML supersedes the earlier no-database pilot: use PostgreSQL from the first durable loop.
- The newer station/domain/deferral proposals remain **Proposed**. Preserve the existing CEL selection contract for this PoC; do not silently implement an alternative station owner rule or a workflow engine. Record this boundary in demo results. Repository-less selection/execution is a separate extension, not a prerequisite for the first repository-bound demo.
- User can provision an external HyperShell gateway. Obtain its exported OpenShell connection configuration and supported client version; the management website is not necessarily the gateway API endpoint. Supply endpoint and credentials locally, without checked-in infrastructure hostnames. Gateway provisioning itself is external to the kind scripts.
- Start with fixture input; the user selected **Jira** as the first live source, matching the worked example and existing adapter.
- Prefer **plain YAML + Kustomize**, with no Helm requirement for the initial PoC. `kubectl apply -k` supports this directly. Use a small PostgreSQL StatefulSet/Service/PVC, not an operator. Consider a pinned upstream Helm chart later only if a dependency earns the additional packaging layer. [Kustomize reference](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/kustomization/).
- No ingress, broker, resident dispatcher, GitOps controller, or cluster-local OpenShell installation is needed for the first demo. Egress to the source, HyperShell, and later Google APIs is sufficient.

## How far this gets us

| Milestone | Demonstrable outcome | External dependency |
| --- | --- | --- |
| 1. Rebuildable environment | Destroy/recreate kind, build/load images, apply config, ready PostgreSQL | Container engine and initial image/tool downloads |
| 2. Durable local loop | Two CronJobs discover fixture work, launch stub Jobs, recover failures, record outcomes | None beyond local Kubernetes |
| 3. Live polling and selection | One real source, current-state fetch, trusted repository context, CEL selection | Source credentials and dedicated test work |
| 4. Remote execution | Runner Job executes `fullsend run` through HyperShell, collects output and cleans up | Provisioned gateway, source/inference credentials |
| 5. Private local mint | Projected Pod identity obtains a repository/role-scoped GitHub token | Test GitHub Apps/installations for the live exchange |
| 6. GCP WIF inference | kind identity obtains Google credentials and performs inference | Dedicated GCP resources, IAM permissions, model access/billing |

Milestones 1–2 are the first implementation session's target. Milestone 4 is a substantial execution PoC, not just YAML. Milestone 5 needs new mint authentication/authorization code. Milestone 6 can start after milestone 1 with an isolated probe Job and need not wait for the mint. None of these proves OpenShift SCC compatibility, production isolation, HA, or exactly-once external effects.

## Files and configuration ownership

Proposed layout; create files as their milestone needs them:

```text
k8s/
  PLAN.md
  README.md                         # quick start, profiles, troubleshooting
  kind/
    deploy.sh                       # ordered calls; deploy is non-destructive
    reset.sh                        # remote cleanup, destroy, then deploy
    destroy.sh                      # explicit owned-resource cleanup + kind delete
    00-preflight.sh                  # dependencies, engine, selected profile inputs
    01-kind.sh                       # create/check named cluster and kubeconfig
    02-images.sh                     # build this checkout, load images into kind
    03-config.sh                     # materialize ignored local config/Secrets
    04-postgres.sh                   # apply storage, wait, run migrations
    05-workloads.sh                  # apply selected workload overlay
    06-smoke.sh                      # run deterministic checks; bounded waits
    status.sh                       # resource summary + redacted diagnostics
    lib.sh                          # tiny shared status/context helpers
    cluster.yaml                    # kind topology and API-server configuration
    versions.env                    # pinned tools/images; no secrets or latest tags
  manifests/
    base/
      infrastructure/               # namespaces, PostgreSQL, storage, RBAC
      control-plane/                # poller/dispatcher CronJobs, config references
      runner/                       # trusted Job template, quota, security settings
    overlays/
      kind-fixture/                 # default; no live credentials
      kind-live/                    # source + external gateway settings
      kind-mint/                    # optional private mint and mint token volume
      kind-gcp/                     # optional inference token volume and ADC config
  config/
    repos.example.yaml              # valid repos.Manifest input
    service.example.yaml            # proposed source/scheduling/launch policy
    local.env.example               # names only for sensitive inputs
  fixtures/                         # sanitized candidates, entities, demo harness
  tests/                            # scenario definitions and integration driver
  gcp/                              # optional declarative IAM/provider resources
  .local/                           # ignored inputs, kubeconfig, generated config
  .state/                           # ignored ownership ledger and redacted reports
```

Kustomize owns resource definitions, patches, schedules, limits, volumes, RBAC, image references, and ConfigMap/Secret wiring. The dispatcher fills only explicit per-attempt fields in a validated runner Job template; it does not accept arbitrary Job YAML from source content. Application behavior, SQL migrations, identity validation, and recovery logic belong in Go, not shell.

Keep one validated application configuration model: reuse `repos.Manifest` for repository/context configuration, following [ADR 0123](../docs/ADRs/0123-tenant-configuration-from-repos-yaml.md). Inspect the current schema before adding the smallest source/context extension; the desired centralized schema is not already implemented. Service-only operational policy may live separately. Kubernetes YAML should package these files, not introduce competing tenant configuration semantics.

Use Kustomize generators for configuration and references. Preserve the exact immutable configuration revision needed by pending selections and Jobs; a stored digest alone cannot reconstruct deleted config. New CronJob invocations pick up new configuration, while admitted runs retain their selected revision. Avoid mounts via `subPath` when relying on mounted file updates. Do not prune old config while referenced.

Ignore `.local/` and `.state/` before creating them. Secrets, app keys, gateway client material, GCP identifiers, internal endpoints, kubeconfigs, and rendered manifests containing those values stay there or in an external supplied input directory. No sensitive defaults in examples. Do not dump rendered Secrets into diagnostics. The default reset preserves local inputs but replaces cluster state.

## Script contract and repeatability

- `k8s/kind/deploy.sh --profile fixture` is the default happy path. Profiles and flags in this plan are proposed script interfaces, not existing commands. Optional mint/GCP capabilities should compose with live execution rather than require copied manifests.
- `k8s/kind/reset.sh --profile fixture` must remove the named PoC cluster and rebuild it completely. Run it twice as acceptance evidence. Database contents are intentionally lost on reset; normal Pod/process restarts must preserve them on the PVC.
- Every step runs independently, resolves paths relative to its own location, uses `set -euo pipefail`, and exits nonzero with the failed step named. Keep orchestration to explicit calls, not a plugin framework. Pin the working container provider; test whichever of Docker/Podman is selected instead of assuming their networking is identical.
- Use a dedicated kubeconfig and explicit `--context`/namespace arguments for every Kubernetes command. Fail if an existing named cluster does not match the owned PoC. Never delete whatever cluster happens to be current.
- `versions.env` pins the kind node image, PostgreSQL, build bases, and OpenShell client. Record source commit and dirty-tree content hash in the run report; build immutable local image tags from the actual inputs and load them into kind. The build must include uncommitted changes. Use `IfNotPresent` or `Never` appropriately for loaded images.
- Check dependencies, configuration, and ability to build/download before destructive reset. Scripts report missing tools and documented install steps rather than silently installing arbitrary versions. `kind`, `kubectl`, Docker, Podman, and `gcloud` were found on this host during planning; engine availability and compatible versions have not been tested.
- Keep database migrations versioned in Go/SQL and run a bounded migration Job before enabling CronJobs. Do not rely on first-start PostgreSQL initialization hooks for upgrades.
- Emit readable status lines such as `🔧 Creating kind cluster`, `⏳ Waiting for PostgreSQL`, `✅ Dispatcher ready`, and `❌ Smoke check failed`. Put status on stderr, reserve stdout for data, honor `NO_COLOR`, disable ANSI on non-TTY output, and support `NO_EMOJI=1`. Centralize only these small helpers in `lib.sh`.
- Use bounded readiness waits and explicit exit conditions, not fixed sleep chains. On failure capture redacted events, Job/Pod state and logs to a report directory with a unique execution ID.
- Before destroying a live profile, suspend scheduling, stop control-plane workers, cancel owned attempts, and clean up **only this PoC's** remote sandboxes/providers/workspaces. Export an ownership ledger before removing PostgreSQL. If HyperShell is unreachable, preserve the ledger and stop destructive reset with an actionable recovery command. An explicit force escape hatch must retain orphan information. Never delete the user-provisioned shared gateway.
- Retain dedicated cloud IAM/provider resources across ordinary kind resets. Refresh their trust material after cluster recreation. Cloud teardown is a separate explicit command; it must never delete shared project resources.

## Milestone 1 — Rebuildable kind skeleton

Deliver the script structure, fixture profile, namespaces, PostgreSQL/PVC, config loading and a purpose-built control-plane image from this checkout. Start with one kind node and modest resource requests; document actual usage after the smoke run. Distinguish control-plane and runner service accounts and namespaces from the start.

Use an in-cluster database Service with readiness probes. Development credentials are generated/supplied outside Git. Resource definitions remain reviewable YAML. Do not claim NetworkPolicy enforcement until the selected CNI demonstrates it; kind's default networking alone is not evidence of tenant isolation.

**Acceptance:** reset twice; deployment converges; a second deploy preserves database data; restarting PostgreSQL preserves a test row; failed prerequisites stop before deletion; every script works from outside the repository root. Record versions and rendered non-sensitive configuration in the smoke report.

## Milestone 2 — Durable polling and dispatch with fixtures

Implement bounded cycle functions around existing Fullsend code. Suggested new packages match the HTML: `internal/service/`, `internal/store/`, and `internal/launcher/kubernetes/`. Extend CLI wiring in `internal/cli/poll.go` and `internal/cli/dispatch.go`; choose final flags during implementation and document them. There is currently no PostgreSQL/Kubernetes dispatch mode to invoke.

```text
fixture source → poller CronJob → PostgreSQL → dispatcher CronJob
                                                 ↓
                                   independent stub runner Jobs
                                                 ↓
                        next dispatch cycle: observe → commit → delete
```

The fixture source uses the same candidate/discovery interfaces intended for the live adapter. Supply current entity/actor fixtures separately from candidate references. Stub runners succeed, fail, or remain active predictably; no LLM or source credentials are required.

Implement the minimum durable records together:

- Source checkpoint plus unique observations, committed atomically. Identity includes tenant, registered source and provider observation key; do not collapse all later changes to the same issue into one candidate.
- Complete selected-run set and retrievable config revision before launch. Record no-match, authorization denial, and evaluation errors distinctly.
- Execution attempts with deterministic Job names, association/UID, outcome and cleanup state. An ambiguous create retries the same attempt; an intentional execution retry creates a new attempt.
- Atomic conflict and capacity reservations under a shared admission guard. Start with global cap 2, per-tenant cap 2, and serialization per tenant/source/entity across harnesses and contexts. Keep waiting work visible. Reservations have no automatic expiry.

Start both CronJobs at one-minute intervals, `concurrencyPolicy: Forbid`, `timeZone: Etc/UTC`, bounded batches and a cycle deadline below the interval. Treat these as tunable demo defaults. Configure worker Job deadlines/history separately from agent Jobs. Manual invocations and duplicate schedule deliveries still require database concurrency protection. [CronJob guarantees](https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/).

Agent Jobs outlive dispatcher invocations. Give them no owner reference to a temporary dispatcher Job/Pod and no automatic TTL. Start with `restartPolicy: Never`, `backoffLimit: 0`, and an explicit execution deadline to avoid invisible retry policy. These settings do not promise exactly-once execution. Record diagnostics and durable outcomes before deleting Jobs with UID preconditions. Unexpected disappearance is an unknown result, not success or permission to rerun.

Each cycle budgets time for reconciliation/cleanup and new admissions so either can progress under load. Keep database transactions short; never hold them around source or Kubernetes calls. Manual smoke triggers must exercise the same cycle functions. Delay deliberate active-active operation until claims/fencing are designed; tolerate accidental overlap without duplicate admission.

**Acceptance scenarios:** one successful run; rediscovery; two revisions of one entity; partial fan-out; no match; invalid selection; DB unavailable; checkpoint commit uncertainty; dispatcher killed before/after Job creation; create response lost; dispatcher down when work ends; outcome committed but deletion fails; missing Job; duplicate manual dispatch; conflict exclusion; capacity exhaustion with unrelated work progressing. Successful siblings must not rerun when another fails. Required failures remain visible until explicit retry/dismissal. Preserve results after Job cleanup.

## Milestone 3 — One live source and trusted selection

Use Jira in a dedicated test scope. Reuse discovery/conversion pieces from `internal/jirapoll/`, normalization from `internal/normevent/`, and authorization/CEL selection from `internal/harnessdispatch/`. The current Jira poll command performs selection and source checkpoint handling; do not wrap it unchanged and treat its JSON output as the new durable producer. Refactor discovery so the centralized path commits candidate responsibility and covered checkpoints together in PostgreSQL. Preserve existing per-repository flows.

Poller submits references only. Dispatcher refetches current entity/activity and actor permissions using registered endpoints, resolves an explicitly configured repository context, checks authorization/kill switches, and evaluates applicable harnesses. Source payloads cannot select tenant, namespace, role, image, shell command, or configuration ref. Timestamp ties, pagination failures, overlap windows and poison candidates need explicit discovery semantics. If the chosen API exposes only updated snapshots, label this a reconciliation poll, not proof of every historical transition.

Reuse `repos.Manifest` and filesystem delivery. Validate a complete config revision before activation. Source/context associations missing from the current schema are implementation work, not shell variables pretending to be supported fields. Keep station/domain migration and repository-less real execution outside this milestone.

**Acceptance:** discover a controlled test issue, create a stub Job through real CEL selection, skip an unauthorized issue, replay safely, process a later revision, survive a failed source page without skipping work, and show a configuration change taking effect for new selections. A broken config is observable and does not become a successful no-match. Source API errors do not starve later eligible work.

## Milestone 4 — Runner Jobs through the provisioned HyperShell gateway

First launch a manual runner Job using the same template the dispatcher will use. Inspect `images/runner/Containerfile`, `internal/cli/run.go`, and `internal/sandbox/` for reusable image, gateway and transfer support. Build the runner from this checkout. Do not assume the existing CI image entrypoint is suitable unchanged.

Use the user-provisioned gateway's connection bundle, CA/client material and compatible OpenShell CLI. `OPENSHELL_GATEWAY_ENDPOINT` exists in the current code, but an endpoint alone does not establish authentication. Verify DNS, TLS, protocol/streaming support, sandbox create/exec, upload/download, provider attachment and deletion from a Pod. Website access was not verified during planning, and no gateway has been provisioned by this session.

Run one small, pinned demo harness through `fullsend run <agent-name>`. Discover any upstream harness definitions at implementation time rather than embedding an inventory here. Start with limited source/inference credentials in Secrets. Keep app signing keys out of runner Jobs. Runner Pod, gateway and sandbox are separate workloads; the runner does not mount the remote sandbox PVC or start a nested container engine.

Assign every remote resource a unique PoC/attempt identity and persist it before effects where possible. Demonstrate two workspaces and per-run credential/provider separation. If the provisioned gateway only offers shared administrative credentials, use a dedicated PoC gateway and record the isolation limitation. Do not describe that shortcut as tenant-safe authentication.

**Acceptance:** input upload, useful execution, validated output download, correct exit status, and cleanup for success/failure/cancellation. Kill a runner and demonstrate orphan reconciliation. Preserve output outside ephemeral Pods before deletion. A terminal runner Job alone must not release conflict/capacity reservations if its remote sandbox might still be running; retain them until stop evidence exists. Reset must clean up owned remote resources before removing the cluster.

## Milestone 5 — Projected identity and a private local mint

This can proceed alongside milestone 4. kind provides Kubernetes service-account tokens; a private mint can trust the cluster's public signing keys without using GCP. This is a new Kubernetes identity path, not a request to weaken GitHub Actions claim checks.

Current `cmd/mint/main.go` pins the GitHub Actions issuer; `internal/mintcore/` authorization expects GitHub workflow/repository claims; `internal/mintclient/` fetches Actions OIDC tokens. Merely changing an issuer URL will not make them accept Kubernetes workloads correctly.

Add an explicit identity source/verifier and separate Kubernetes authorization policy. Trust only operator-configured issuer/keys. Verify signature, algorithm, issuer, mint audience, validity, and consistency between subject and Kubernetes namespace/service-account claims. Follow the HTML's namespace → tenant/installations/role ceilings binding and strict repository-ID/role service-account naming convention. Requests cannot broaden that signed identity's repository or role. GitHub token requests must specify repository IDs and permissions, never accidentally request installation-wide access.

Export the cluster's public JWKS during bootstrap and mount it into the private mint's trust configuration. Refresh it on reset; never export the API server's signing private key. Start with mocked GitHub responses for negative tests, then a test App installation. Keep App PEMs only at the mint. Preserve existing platform adapters and required mint embedding sync when changing mintcore.

Launcher idempotently ensures the approved repository/role service accounts, rejects conflicting existing accounts, and reuses them across Jobs. Project a short-lived mint-audience token only into the trusted runner; disable automatic Kubernetes API token mounting for agent runners. The launcher has its own narrowly scoped API identity. RBAC allowing Job creation implicitly allows service-account use: validate/admit the permitted Job spec and keep untrusted agents out of that capability.

Reread rotated token files on exchange; do not cache the first JWT for the run. Add forge-token refresh and atomically update the credential used by the remote sandbox. Environment mutation in the runner cannot refresh an already running agent. The mint follows the design's standing identity authority; it does not query Jobs or dispatch admission records.

**Acceptance:** authorized repository token works; wrong issuer/audience/namespace/role/repository and expired tokens fail; repeated rotation succeeds; refresh denial fails closed. No projected mint JWT or App key reaches the sandbox/logs. Offline verification cannot immediately revoke a deleted Pod's JWT, and minted tokens have their own lifetime; explicitly demonstrate and record that bound. [Kubernetes token validation](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/).

## Milestone 6 — GCP WIF and inference from kind

**Yes, this is feasible without exposing kind publicly.** Google documents federation from self-hosted Kubernetes with uploaded JWKS, projected tokens and file-based external-account credentials. Use a dedicated pool/provider, restrictive subject/namespace conditions, and service-account impersonation where appropriate. [Google Kubernetes federation guide](https://docs.cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes).

```text
Pod's inference-audience token → Google STS → IAM service account → inference API
Pod's mint-audience token      → private mint → scoped GitHub installation token
```

These are independent trust paths and projected token files. GKE's metadata-server integration is unnecessary. A cloud-side pool/provider, permission grants, API enablement, network egress and model access are still required. Google supports federation for authentication outside its cloud; validate the selected inference endpoint and model with an actual request. [Inference authentication](https://docs.cloud.google.com/vertex-ai/docs/authentication).

For this PoC, prefer a small declarative OpenTofu/Terraform module under `k8s/gcp/` over a growing set of imperative IAM shell calls. Pin the tool/provider, use ignored local variable/state files, and limit resources to a dedicated development pool/provider and inference service account. Keep this optional so default reset needs neither cloud credentials nor cloud permissions. Feed the exported public JWKS into the provider and replace its trusted keys after every kind recreation; a stable issuer name does not imply stable keys. Report stale trust explicitly and wait for propagation with a bound.

Start with a standalone in-cluster credential/inference probe. Project the audience expected by the configured provider, mount generated external-account ADC configuration, and use the exact service-account subject allowed by IAM. Verify a harmless inference call, then token rotation, then rejection of a second ungranted service account. Do not print tokens or introduce a service-account JSON key fallback. This proposal builds on the documented flow; it has not been tested on this kind installation.

**Separate integration gate: the inference client may run in the remote HyperShell sandbox.** A projected volume on the runner is not automatically available there, and kubelet rotation does not update a copied remote file. Current `run.go` can accept a prepared `GOOGLE_APPLICATION_CREDENTIALS` file but its OIDC refresher fetches from an Actions-style endpoint. Inspect the chosen runtime/provider before reusing that path.

Preferred integration: keep the projected assertion/exchange in the trusted runner or an authorized inference gateway, and refresh a run-scoped inference provider credential usable by the sandbox. If the supported runtime instead requires external-account ADC inside the sandbox, explicitly implement secure delivery and atomic refresh of a **separate inference-only assertion** with narrowly limited IAM authority. This deliberately delegates inference identity to that sandbox; document the boundary. Never copy the mint assertion, API token or gateway administrative credentials. Verify refresh with the actual running inference client, not just the probe Job.

**Acceptance:** real inference from a Pod; successful authentication after cluster destroy/recreate and key refresh; wrong identity denied; then a representative Fullsend/HyperShell run continues across credential rotation without static inference keys. If cloud organization policy prevents creating a provider or granting IAM authority, retain the executable local probe and record the specific external blocker rather than claiming kind cannot federate.

## Execution and verification guidance for the next session

Implement milestones 1–2 first, with fixtures and stub Jobs. Read repository guidance for Go, shell, forge abstraction and runtime changes as applicable. All application forge operations go through the existing forge abstraction; do not implement the live source in shell. When changing Go production code, verify patch coverage (80% target, 75% floor), not only passing tests. Use meaningful transaction/launcher/recovery tests and actual kind scenarios.

Before running `make lint`, stage only the intended changes. Update CLI/config references and related guides whenever implementing new flags or fields. Use `go run ./cmd/fullsend …` for host-side CLI commands. Container images must build the same checkout. No commit, push, deployment, cloud resource creation or source mutation was performed while writing this plan.

Maintain a small evidence report per milestone: source/build revision, versions, command, expected behavior, observed outcome and remaining limitations. The smoke command must be noninteractive, terminate on failure, and return a useful exit status. Keep manual trigger commands so the demo does not require waiting for scheduled minutes.

Open inputs for later milestones: Jira test scope; actual gateway endpoint and connection bundle; approved test repository/App installations; cloud development resource identifiers and permission grants. These do not block the fixture milestones. Leave webhooks, continuous/active-active workers, general tenant onboarding, station/domain migration, production PostgreSQL operations and OpenShift validation for follow-up work.
