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
interface was subsequently implemented with fixture coverage; see
[delivery recovery](development.md#jira-delivery-recovery). It was not part of
this October 9 live simulation.

The October 9 simulation used public text/HTML/Markdown and local Markdown/plain
text. Configured local PDF ingestion was subsequently implemented with synthetic
fixture coverage, page citations, and isolated extraction; see
[file evidence setup](development.md#vendor-sources-and-systems-runbooks). Real
private PDFs, authenticated vendor portals, arbitrary search, real Jira
permissions/webhook wiring, and real Google Workspace execution were not tested. The next batches are recorded in
[the pilot plan](pilot-plan.md#follow-on-batch).

## Delivery recovery verification (October 17, 2026)

The recovery follow-up passed `go test -race -count=1 ./...` with disposable
PostgreSQL 17 and migrations 1–10, `go vet ./...`, both binary builds, and all
25 frontend tests. Additional integration coverage verifies definite-rejection
recovery and transactional retry history. Fixtures cover stale revisions,
concurrent resend/claim decisions, verification racing a resend, requester-reply
supersession, restart continuity, audit rollback, legacy migration behavior,
comment author/body/time/correlation mismatches, and bounded failure handling.

Headless renders of the shipped console using service-response fixtures showed
no horizontal document or dialog overflow at widths 390 and 1200. Interactive
browser automation was unavailable during this check. No real Jira tenant or
Google mutations were involved; actual Jira property round-tripping and bot
permissions remain pilot validation work.


## Deterministic vendor scoring verification (October 17, 2026)

The vendor-scoring follow-up passed the full Go race suite with disposable
PostgreSQL 17 and migrations 1–10, vet, both binary builds, and all 28 frontend
tests. Twelve synthetic evaluation fixtures cover rating boundaries, incomplete
scope, unknown assessments, failed/truncated/omitted sources, historical evidence,
and vendor claims that cannot satisfy independent assurance requirements.

Provider tests cover structured and ordinary JSON contracts; database/API tests
verify current evidence, frozen rubric authority, report round-tripping, source
claim labels, SYSTEM scoring audit events, and the computed requester draft.
Desktop (1200px) and mobile (390px) headless renders of the shipped console with
synthetic fixtures showed no horizontal document or dialog overflow.

This verifies implementation behavior, not expert calibration or live-model
semantic accuracy. The example rubric and synthetic expected assessments still
need human review against actual deployment requirements. No Workspace tenant,
Google writes, private vendor artifacts, or paid model calls were needed.

## Configured file evidence verification (October 19, 2026)

The file-evidence follow-up passed the full Go suite with the race detector and
disposable PostgreSQL 17, `go vet ./...`, both binary builds, and all 29 console
tests. Additional focused race runs covered the final PDF fixtures and report
changes. Production Poppler/Bubblewrap extraction handled readable, scanned,
encrypted, malformed, over-page-limit, over-file-size, and excerpt-limited
synthetic documents. Process tests checked cancellation, bounded output, stripped
server credentials, and filesystem isolation. Escaping symlinks and FIFOs were
rejected by confined file reads.

Persisted investigation tests exercised readable/scanned/encrypted/dense PDF
reports: complete evidence retained deterministic scoring; incomplete evidence
withheld scores and kept requester messages as reviewable drafts. Audit metadata
retained page/file provenance without paths or document bodies. Desktop and
390px headless console checks displayed page citations, original-file hashes,
affected page numbers, and extraction gaps without horizontal overflow. The
collaborative browser snapshot timed out twice, so this visual check used the
shipped HTML/CSS/JS with synthetic API responses in the headless preview.

No real private document, external model, Jira tenant, or Google Workspace was
used for this follow-up. Fixture-based extraction does not establish semantic
accuracy for complex layouts, image text, tables, or real assurance reports;
operator calibration against permitted real documents remains necessary.

## Operator reliability verification (October 19, 2026)

The combined batch passed `scripts/verify.sh` against disposable PostgreSQL 17:
vet, the complete Go race suite, all 38 console tests, three binary builds, and
the seven-case recorded evaluation baseline. A populated legacy-schema upgrade
test backfills case roots and preserves original IDs, evidence provenance,
reports, and delivery uncertainty. Fresh and upgraded schemas require version 12.

Case-control fixtures cover command replay/conflicts, concurrent retries,
late-result rejection after cancellation, unsent reviewed Jira supersession,
internal questions/replies, fresh linked attempts, and exhausted workers when no
other job is claimable. Worker claim/takeover and writes now share the case lock;
a blocked stale writer rechecks its lease after acquiring that lock. A dedicated
regression test expires the old lease while its writer waits and proves it cannot
commit evidence or bypass the held case lock.

The stateful synthetic connector fixtures cover approval before queuing, expired
grants, exactly bound arguments, version preconditions, independent membership
and operation-receipt reads, three crash boundaries, stale-worker fencing,
read-only recovery, bounded claims, disabled pilot endpoints, and surviving
process-component recreation. The model-comparison fixture exercises three model
IDs through the actual HTTP provider protocol, including rejected wording and
invalid citations. Evaluation authority is frozen before calling a provider, and
oversized responses fail the shared report boundary/capture budget.

A running Wardstone process with a local model fixture and a separate PostgreSQL
database completed console intake, an internal reply, a failed-attempt retry,
cancellation during a slow model call, successful approved synthetic execution,
and stale-proposal rejection. Desktop/mobile visual checks used shipped assets
and responses captured from that service; forms, attempt history, approval
arguments/digests, execution outcomes, and recovery controls rendered without
horizontal overflow. Collaborative preview snapshots timed out twice, so visual
inspection used the headless preview. Internal-only drafts no longer offer Jira
send controls, and long mobile case IDs no longer crowd the close button.

These checks used synthetic data and local provider fixtures. No production
service/database was migrated, and no real Jira, Google Workspace mutation, or
paid provider call was made for this batch. See the
[upgrade notes](release-2026-10-19.md) and [operator guide](operator-guide.md).

## Evidence inspector verification (October 24, 2026)

The read-only source inspector passed `scripts/verify.sh` with disposable
PostgreSQL 17: vet, the full Go race suite, all 46 console tests, three binary
builds, and the seven-case offline evaluation baseline. New fixtures cover exact
attempt isolation (including linked retries), deleted source-file snapshots,
observations older than the model-memory bound, PDF page/gap metadata, unknown or
mismatched provenance, bounded payloads, field projection, and operator-only
authentication. Excerpts remain absent from audit events.

A running Wardstone service with synthetic stored research was exercised in
headless Chromium at widths 1200 and 390. Source text remained literal, long
tokens produced no horizontal document/dialog/source overflow, and inspection
made no external requests. Escape/reopen, cache disposal, and disconnect cleanup
passed; a queued native-close race found during this check now has a regression
test. The collaborative desktop browser was disconnected, so these checks used
local headless Chromium. No production database, real private documents, Jira,
Workspace, or model-provider calls were used. This validates inspection behavior,
not whether a passage semantically supports a model's claim.
