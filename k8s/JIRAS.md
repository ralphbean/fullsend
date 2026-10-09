# AISDLC Jira relevance

This note maps the Features and Initiatives under [AISDLC-37, Centralizing on Openshift](https://redhat.atlassian.net/browse/AISDLC-37), to the current local Kubernetes dispatcher PoC described in [README.md](README.md), [PLAN.md](PLAN.md), and [STATUS.md](STATUS.md). Jira statuses below were observed on 2026-10-07.

## Closest matches

| Issue | Relevance to the current PoC |
| --- | --- |
| [AISDLC-65 — Poll and dispatch managed Fullsend runs](https://redhat.atlassian.net/browse/AISDLC-65) (New) | Closest functional match. The PoC implements polling, selection, persistence, and Kubernetes Job dispatch. It remains incomplete against the feature: the dispatcher launches stub Jobs rather than real `fullsend run` Jobs. The PoC also uses PostgreSQL, while the feature's pilot success criteria call for a single poller and dispatcher that do not require a database or broker. |
| [AISDLC-63 — PoC Fullsend Run on OpenShift through remote OpenShell gateway](https://redhat.atlassian.net/browse/AISDLC-63) (New) | Partial execution-path overlap. The PoC has manual `fullsend run` Jobs using a remote HyperShell gateway, with evidence for output transfer and sandbox cleanup. It runs in local kind and does not establish the feature's OpenShift Job, restricted SCC, or Kubernetes-backed OpenShell gateway path. |
| [AISDLC-64 — PoC Workload Identity Federation from OpenShift](https://redhat.atlassian.net/browse/AISDLC-64) (New) | Partial identity-path overlap. The PoC demonstrates GCP WIF inference from a kind Pod and a manual run, but does not demonstrate federation from OpenShift. Wrong-identity denial and other trust-boundary acceptance checks remain open. |
| [AISDLC-57 — Tenant Configuration with repos.Manifest](https://redhat.atlassian.net/browse/AISDLC-57) (New) | Related configuration overlap. The plan uses `repos.Manifest` for repository configuration, but this PoC does not implement the feature's repository-less tenant contexts or its manifest extension. |
| [AISDLC-56 — Managed Fullsend central service MVP](https://redhat.atlassian.net/browse/AISDLC-56) (Closed) | Umbrella context for the managed service. The PoC explores parts of the service architecture and execution path, but does not deliver the managed pilot described by the issue. |

## Related follow-up or separate work

| Issue | Relationship to the current PoC |
| --- | --- |
| [AISDLC-146 — Run Fullsend agents without a target repository](https://redhat.atlassian.net/browse/AISDLC-146) (New) | The plan explicitly treats repository-less execution as a separate extension and not a prerequisite for the current repository-bound PoC. |
| [AISDLC-66 — Receive webhooks for managed Fullsend dispatch](https://redhat.atlassian.net/browse/AISDLC-66) (New) | The PoC polls Jira; it does not implement webhook delivery or a webhook receiver. |
| [AISDLC-147 — Deploy managed Fullsend polling and dispatch active-active](https://redhat.atlassian.net/browse/AISDLC-147) (New) | Active-active poller or dispatcher replicas are not implemented. The plan identifies them as follow-up work. |
| [AISDLC-230 — Provision dev and prod ROSA HCP clusters for Fullsend operations](https://redhat.atlassian.net/browse/AISDLC-230) (New) | Relevant to future managed-service infrastructure, but separate from this PoC's local kind environment. The current results do not establish OpenShift compatibility. |

## Not addressed by this PoC

| Issue | Why it is outside the current PoC |
| --- | --- |
| [AISDLC-70 — Onboard and authorize managed Fullsend tenants](https://redhat.atlassian.net/browse/AISDLC-70) (New) | The live profile exercises Jira issue labels and project-role checks; it does not define or demonstrate tenant onboarding, tenant identity, or tenant administrator access. |
| [AISDLC-143 — Define centralized data source and MCP access models](https://redhat.atlassian.net/browse/AISDLC-143) (New) | The PoC does not define tenant access to restricted data sources, credentialed MCP tools, or their authorization and credential lifecycle. |
| [AISDLC-144 — Bring layered configuration into line with ADR 0069](https://redhat.atlassian.net/browse/AISDLC-144) (In Progress) | This issue concerns per-repository configuration layers and precedence. The dispatcher PoC's use of `repos.Manifest` is not implementation of those configuration-layer changes. |

## Current implementation boundary

The docs describe a local kind PoC. Milestones 1–3 cover the cluster skeleton, durable fixture loop, and live Jira selection. Milestone 4 has manual HyperShell `fullsend run` evidence, while the Jira dispatcher still launches stubs. Milestone 5's private Kubernetes-identity mint and Milestone 6's GCP WIF inference path are verified on kind. The results do not establish OpenShift compatibility, production tenant isolation, active-active operation, or successful dispatcher-to-real-run integration.
