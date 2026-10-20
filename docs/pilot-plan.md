# Jira specialist pilot

This batch makes the existing durable case lifecycle useful without requiring a
real Jira tenant or Google Workspace. Production code and repeatable tests live
in the repository; credentials, mock servers, live-model transcripts, and the
temporary database environment stay outside Git.

## Batch 1: reviewed requester communication

Status: implemented and verified.

- Add a REVIEW operating mode: investigations create drafts, an authenticated
  operator approves exact message text, and only approved text enters the outbox.
- Retain original drafts and record review identity, time, and draft digest.
- Show review controls and delivery status in the web console.
- Keep infrastructure mutations denied in SHADOW and REVIEW.
- Test authentication, stale/superseded drafts, concurrent review, edited text,
  restart continuity, and delivery deduplication at the enqueue boundary.

## Batch 2: evidence for specialists

Status: implemented and verified.

- Accept WARDSTONE_AI_KEY as an OpenRouter configuration shortcut while retaining
  explicit OpenAI-compatible endpoint/model/key configuration.
- Add administrator-configured public vendor sources and local systems runbooks.
  Ticket fields or exact authenticated replies select configured names; ticket
  URLs never become fetch targets. Reads are bounded and auditable.
- Return cited executive summaries and an explicit provisional vendor risk
  assessment, with unknowns and the distinction between vendor claims and facts.
- Exercise help desk, access management, vendor review, and systems knowledge.
  Google absence must produce uncertainty/escalation rather than invented access.

## Batch 3: verification and live simulation

Status: completed. See [pilot results](pilot-results.md).

- Build both binaries; run Go unit/integration tests, race checks, vet, and the
  frontend tests using disposable Docker PostgreSQL.
- Run synthetic Jira intake/replies against the real service and a live
  OpenRouter model, with a simulated Jira comment API and public Tailscale sources.
- Inspect tool invocations, citations, routing, handoffs, reports, reviewed
  delivery, duplicate webhooks/replies, and continuity across service restarts.
- Record sanitized outcomes and limits in docs/pilot-results.md.

## Jira delivery recovery

Implemented after the initial pilot: operator-triggered Jira comment verification,
explicit resends of unchanged reviewed text, separate uncertain/failed/superseded
states, revision-checked recovery decisions, and durable send/recovery audit
history. Recovery uses the existing single-administrator authentication. Database,
connector, and console fixtures cover crash recovery and concurrent decisions;
real Jira tenant verification remains a pilot task. See
[recovery setup and API](development.md#jira-delivery-recovery).

## Deterministic vendor scoring

Implemented after delivery recovery: Go computes ratings from a configurable
versioned rubric and administrator deployment context. Structured criterion
assessments carry citations; missing scope, sources, or assurance remain visible
gaps and withhold a numeric score. Reports retain immutable rubric snapshots,
SYSTEM scoring audit events, and attributed vendor claims. Go generates the
requester-summary rating text. Twelve synthetic evaluation scenarios and
provider/database/console tests cover the implementation. Human calibration of
the example rubric and review of the seed evaluation set remain outstanding.
See [vendor risk scoring](development.md#vendor-risk-scoring).

## Configured file evidence

Implemented after deterministic scoring: administrator-configured PDF, Markdown,
and text files for vendor and knowledge catalogs. PDF pages have distinct
citations and original-file hashes; isolated extraction is bounded by file size,
page count, time, memory, CPU, output, and excerpt budgets. Scanned/empty-text,
encrypted, malformed, missing, and truncated evidence remain visible gaps and
withhold vendor scores. Synthetic fixtures cover the collector, persisted
reports, reviewed drafts, and console. Real private-report calibration and OCR
remain outstanding. See [file evidence setup](development.md#vendor-sources-and-systems-runbooks).

## Operator reliability batch

Implemented after file evidence: console case creation, internal replies,
durable cancellation, linked fresh retries, and exhausted-worker recovery;
an opt-in synthetic PostgreSQL group connector with exact approvals, durable
execution phases, idempotent crash recovery, and independent verification; and
a bounded saved-final-report evaluation CLI sharing production report/scoring
validation. See the [operator guide](operator-guide.md) and
[upgrade notes](release-2026-10-19.md). Real infrastructure executors and broader
operating modes remain disabled.

## Follow-on batch

1. Review and calibrate the implemented vendor rubric and seed evaluation set
   against actual deployment requirements and reviewed source interpretations.
   Expand semantic wording evaluations and validate delivery recovery against a
   real Jira tenant before expanding requester delivery.
2. Extend the implemented synthetic execution boundary to one approved Google
   group addition after validating tenant permissions, current-state preconditions,
   and Google-specific idempotency/unknown-outcome recovery. The synthetic connector
   already exercises approval, execution, restart recovery, and independent reads;
   real infrastructure requires its own connector-specific verification.
3. Add Slack, managed credential lifecycle, and broader routing/data controls
   after those workflows are established. Expand autonomous execution only after
   recovery and adversarial tests cover each permitted capability.

This batch does not enable APPROVAL or AUTONOMOUS runtime modes.
