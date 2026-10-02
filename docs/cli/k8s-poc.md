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
| `retry RUN_ID` | Requeue a failed run after its Job has been cleaned up. The next dispatch makes attempt 2, 3, and so on. |
| `dismiss RUN_ID` | Acknowledge a cleaned-up failed run without rerunning it. |

`migrate`, `poll-fixture`, `poll-jira`, `dispatch`, `status`, `retry`, and `dismiss` require `DATABASE_URL`. `poll-fixture` accepts `--fixture` (default `/etc/fullsend/fixture.json`) and `--selection` (default `/etc/fullsend/selection.json`). `poll-jira` accepts `--config` (default `/etc/fullsend/live.json`), `--jira-token-file` (default `/var/run/secrets/jira/token`), and `--jira-email-file` (default `/var/run/secrets/jira/email`). `prepare-live` requires `--repos-manifest`, `--repo-dir`, `--repo`, `--jira-url`, `--jira-project`, `--jira-label`, and `--output`. The repository must be registered in the manifest and contain a loadable `.fullsend/config.yaml` with triggered harnesses. `dispatch` uses `STUB_IMAGE`, `JIRA_TOKEN_FILE`, and `JIRA_EMAIL_FILE` in the live profile, plus an in-cluster service account to create runner Jobs. The kind scripts supply these settings locally.
