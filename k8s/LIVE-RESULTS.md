# Jira live milestone results

Observed 2026-10-02 with the rootless Podman kind cluster from [RESULTS.md](RESULTS.md). Source revision was `90833a878e47000526d31f3ea3a2cd258209f4d2` with uncommitted changes included in the last deployed content-derived image tag `90833a878e47-313f11ff9a9cdada`. The control and stub images used pinned Red Hat UBI 10; PostgreSQL used the pinned 17.6 image. The selected repository checkout was `ralphbean/jira-triage-test` at `d8cb465`. Jira source scope was project `FSENDAI` with label `fullsend-dispatcher-test`.

| Check | Expected | Observed |
| --- | --- | --- |
| `kind/deploy.sh --profile live` | Validate config, install Jira CronJobs, and trigger a bounded cycle | Passed; final ignored smoke report `live-smoke-20261002210638-2121673` |
| New labeled Task `FSENDAI-5516` | Discover, refetch, authorize, CEL-select `triage`, and launch stub | `opened` observation selected; run 10, Job `fs-poc-10-1`, Kubernetes UID recorded, outcome `succeeded`, cleanup `done` |
| `/fs-triage` comment on the same issue | Produce a later distinct revision | `comment_added` observation selected; run 11, Job `fs-poc-11-1`, Kubernetes UID recorded, outcome `succeeded`, cleanup `done` after another cycle |
| Repeated live poll | Preserve one observation and run per Jira event | Two observations and two runs remained after repeated smoke cycles |
| Database integration tests | Deny unauthorized actors, replay without duplicates, survive a failed issue page and refetch, reject scope loss and corrupt checkpoints, preserve configuration revisions, reject partial harness revisions, and exercise the real Jira HTTP client against a local server | Passed with the race detector against isolated schemas in kind PostgreSQL; `internal/k8spoc` coverage 81.7% |
| Approximate changed-line coverage | Meet the 75% enforced floor; aim for 80% | 75.2% across changed Go production files from combined focused and suite profiles; above the floor, below the target |
| Fixture integration regression | Preserve milestone 2 behavior | `TestDurableFixtureCycle` passed against an isolated schema in the same instance |

The live test issue is a Task, and the repository's existing Jira GitHub Actions poll workflow is restricted to Bugs. The PoC dispatcher only creates stub Jobs in `fullsend-runner`; it does not run the repository's real agent or modify Jira. The test repository's existing content was therefore compatible with the demo. The shared test label limits source search, while its pinned harness trigger selects `opened` and `/fs-triage` comment events.

The unauthorized actor, failed issue page, and changed CEL revision cases were exercised with a fake Jira client and real PostgreSQL, not by creating additional Jira issues. Jira search returns current issue snapshots; the poller cannot demonstrate every historical transition. There is no HyperShell execution, mint, GCP inference, active-active fencing, or production isolation claim in this milestone.
