# Jira specialist pilot results

Verified on October 9, 2026 using a disposable Docker PostgreSQL 17 database,
synthetic Jira intake/comment endpoints, a local fictional VPN runbook, and live
OpenRouter `openai/gpt-4.1-mini` calls. The temporary harness, database credentials,
and transcripts were kept outside the repository. No real Jira tenant or Google
Workspace was used. Tailscale sources were fetched from its configured public
HTTPS pages using the production research collector.

## Delivered behavior

The durable workflow is: authenticated Jira intake → deterministic routing or
bounded intent fallback → permitted evidence collectors → validated model
question, handoff, or work package → policy evaluation → operator review of the
exact requester message → durable Jira delivery. An authenticated requester
reply resumes the same case, including after a service restart. Every stage is
audited. SHADOW stores drafts; REVIEW enables reviewed comments while continuing
to deny infrastructure mutations.

Vendor and systems collectors read only administrator-configured sources.
Evidence retains source title, URL when applicable, content hash, observation
time, truncation, and claim type. Reports cite actual evidence IDs. Vendor
reports also include an explicit provisional risk rating and rationale. Missing
Google credentials produce an operator escalation and unknown membership.

## Live scenarios

All seven core scenarios reached COMPLETED. Completion means an investigation
report was persisted; it does not mean a proposed infrastructure change ran.

| Scenario | Evidence/tool path | Observed result |
| --- | --- | --- |
| Mapped access request, no Google credentials | Local `google.access.inspect` configuration observation; no Directory call | Unknown membership, operator escalation, no proposed mutation |
| Tailscale executive vendor review | `research.vendor.collect`, three configured public documents | Cited executive summary, gaps, provisional MODERATE risk |
| Systems troubleshooting question | `research.knowledge.collect`, synthetic VPN runbook | Cited recommended steps and escalation conditions; no execution claim |
| Vague help desk request | Intent fallback, edited/reviewed question, restart, authenticated reply, systems handoff, exact runbook selector | Same case completed with cited troubleshooting report; initial routing retained |
| Vendor name initially missing | Reviewed clarification, exact whole-reply `Tailscale`, resumed research | Cited report with provisional LOW risk; see calibration limits below |
| Unconfigured vendor with a metadata URL in ticket text | Catalog selector observation only | UNKNOWN risk and operator escalation; ticket URL never fetched |
| Access request with read-only Google fixtures | Clarification, restart, user/group/direct-membership reads | Valid `google.groups.add_member` proposal with exact `group`/`user` arguments; denied with `review_mode`; zero Google writes |

The core run made **13 live model calls**, delivered **11 reviewed comments** to
the simulated Jira API, and made seven read-only Google fixture requests. Its
reported OpenRouter cost was **$0.0225724**; this excludes earlier development
attempts and the separate browser smoke case. Duplicate intake/replies and
identical review retries did not create additional work or comments.

A separate live browser case completed a systems investigation, edited and
approved its message in the console, and verified SENT with a simulated Jira
remote comment ID. The API retained both the original draft and the exact
approved text. Desktop and 390 × 844 mobile checks found no horizontal document
overflow. This added one model call and one reviewed comment to the core run.

## Repeatable checks

- Service and terminal-console binaries build successfully.
- `go vet ./...` passes.
- `go test -race -count=1 ./...` passes with
  `WARDSTONE_TEST_DATABASE_URL` pointing to Docker PostgreSQL. Persistence and
  API/worker tests create isolated schemas and apply migrations 1–9.
- `node --test internal/api/web_app_test.js`: 19 passed, zero skipped or failed.
- `git diff --check` passes.

The committed integration fixtures cover authentication and mode gating,
concurrent idempotent review, edited-message continuity after service
reconstruction, immutable reviews, stale/superseded drafts, pending-send
suppression, duplicate replies, specialist report persistence, cited risk, and
expired reviewed-send reconciliation. Unit tests also exercise source selection,
download/excerpt bounds, unsafe network targets, partial evidence failure,
OpenRouter configuration/budgets, strict result variants, exact capability
argument contracts, and supplied-ID citation enums.

## Limits and next work

The live model exposed several useful integration failures during development:
mixed question/report shapes, repeated clarification after an operator-only
configuration gap, incorrect action argument names, and content hashes used as
citation IDs. The final workflow uses strict disjoint response variants,
current-run selector constraints, registered action argument schemas, and
supplied evidence ID enums. Runtime validation remains authoritative.

These checks establish message structure and provenance, not semantic truth.
The two Tailscale runs returned LOW and MODERATE under different stated scope
assumptions; ratings are not yet calibrated. Model text also called SOC 2 a
"certification" and sometimes stated vendor claims too strongly. Public pages
do not independently verify controls. Private audit reports, deployment
requirements, and actual tenant configuration were unavailable; the DPA text
exceeded the excerpt budget and was marked truncated. Operators must review
claim wording and risk rationale before sending a report.

Jira delivery is not exactly once at the remote API boundary. Ambiguous POST
outcomes and expired running sends stop for reconciliation; only explicit
rate-limit responses are automatically retried. A manual reconciliation/resend
interface is still needed.

Research currently supports public text/HTML/Markdown and configured local
Markdown/plain-text files. PDF parsing, authenticated vendor portals, arbitrary
search, real Jira permissions/webhook wiring, and real Google Workspace
execution were not tested. The next batches are recorded in
[the pilot plan](pilot-plan.md#follow-on-batch).
