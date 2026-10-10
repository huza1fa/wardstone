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

## Follow-on batch

1. Calibrate vendor risk with an explicit scoring rubric, required deployment
   assumptions, and a small reviewed evaluation set. Validate claim wording as
   well as citation identity; distinguish SOC 2 attestations from certifications.
   Add operator-visible evidence gaps and a reconciliation workflow for uncertain
   Jira sends before expanding requester delivery.
2. Implement one approved Google group addition with action-digest binding,
   freshness recheck, durable execution state, and an independent membership read
   proving the postcondition. Exercise crash recovery with fixtures first, then
   validate permissions and behavior in a suitable Workspace test tenant.
3. Add Slack, managed credential lifecycle, and broader routing/data controls
   after those workflows are established. Expand autonomous execution only after
   recovery and adversarial tests cover each permitted capability.

This batch does not enable APPROVAL or AUTONOMOUS runtime modes.
