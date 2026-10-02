# Fixture milestone results

Observed 2026-10-02 on a rootless Podman 6.1.1 host with kind 0.23.0. Source revision: `90833a878e47000526d31f3ea3a2cd258209f4d2`; second reset image tag: `90833a878e47-8724b917fc1661e5` (dirty-tree hash `8724b917fc1661e5`). Images: `docker.io/kindest/node:v1.30.0`, `docker.io/library/postgres:17.6`, and `registry.access.redhat.com/ubi10/ubi-minimal:10.2-1790075626` for the control plane and stub runner.

| Check | Expected | Observed |
| --- | --- | --- |
| `kind/reset.sh --profile fixture` twice | Fresh cluster, migration, and fixture smoke each time | Both final resets passed; reports `smoke-20261002192003-1792069` and `smoke-20261002192708-1807377` under ignored `.state/` |
| Fixture smoke each reset | 4 observations, 1 success, 1 intentional failure, 1 no-match, 1 denial | `4|1|1|1|1` both times; runner Jobs were removed |
| Repeated `kind/deploy.sh --profile fixture` | Preserve database contents | Test row remained present |
| `kind/deploy.sh --profile fixture` from `/tmp` | Resolve paths outside the checkout | Passed; report `smoke-20261002193139-1816004` observed `4|1|1|1|1` |
| PostgreSQL Pod deletion and recreation | Preserve PVC contents | Same test row remained present |
| Invalid provider prerequisite | Stop before deleting cluster | Reset exited nonzero; owned cluster remained healthy |
| Database-backed Go tests | Rediscovery, revision conflict, cap, partial fan-out, missing Job, lost create response, delete failure, retry/dismiss | Passed with race detector; `internal/k8spoc` coverage 81.9%; new CLI command function 100% covered |
| `make go-test` and staged `make lint` | Pass | Passed; full Go suite reported 82.8% total coverage |

The final smoke report recorded one kind-node usage snapshot of 65.26% CPU and 558.3 MB memory. This is a transient node measurement, not an application capacity estimate.

Fixture selection maps candidate kinds to stub harnesses. It does not implement the live Jira/CEL selection path, HyperShell execution, projected identity, GCP federation, active-active fencing, or production tenant isolation. The tests exercise ambiguous creates and missing Jobs through an API simulator; process kill and source checkpoint commit uncertainty have not been fault-injected in kind. kind's default CNI is not evidence of NetworkPolicy enforcement. No live e2e suite or external source mutation was run for this credential-free milestone.
