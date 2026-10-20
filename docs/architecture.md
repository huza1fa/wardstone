# Architecture

## Scope and shape

Wardstone starts as a modular monolith. One Go process owns the HTTP control
plane, orchestration workers, deterministic policy evaluation, and connector
adapters. PostgreSQL is the durable source of truth. Package boundaries keep
privileged execution separate from model-driven investigation without adding
network boundaries that do not yet provide value.

The dependency direction is inward: transport, PostgreSQL, model providers,
and connectors implement small interfaces consumed by the application core.
The core does not depend on HTTP payloads, vendor SDK types, or a particular
model API.

Implementation packages live under `internal/`. No public `pkg/` API is exposed
until an external connector/plugin contract is stable enough to support.

The intended dispatcher/specialist control model, trust boundary, and human
escalation principles are defined in the [operating model](operating-model.md).
This architecture describes the shared runtime that enforces that model;
specialists must not introduce separate orchestration or authority paths.

## Modules

| Package | Responsibility |
| --- | --- |
| `internal/admin` | Transport-neutral operator read models shared by web and terminal views |
| `internal/api` | HTTP control plane, admin API, embedded web console, input validation, status and timeline views |
| `internal/config` | Environment configuration and validation |
| `internal/database` | PostgreSQL pool and durable repository implementations |
| `internal/audit` | Append-only event types and timeline access |
| `internal/agent` | Model-neutral diagnosis and action-proposal contract |
| `internal/models` | Model provider implementations and wire formats |
| `internal/connectors/*` | Jira, Google Workspace, and Slack adapters |
| `internal/capabilities` | Capability metadata registry and action validation |
| `internal/policy` | Deterministic policy evaluation |
| `internal/approvals` | Approval lifecycle and exact-action binding |
| `internal/executor` | The only path to privileged mutating connector methods |
| `internal/investigations` | Ordered workflow and concurrent evidence collection |
| `internal/specialists` | Immutable specialist profiles and deterministic, least-privilege dispatcher |
| `internal/worker` | Bounded workers over durably leased jobs |
| `internal/sandbox` | Unprivileged research runtime contract |
| `internal/tui` | Charm terminal client and presentation model over the operator API |
| `internal/domain` | Domain types and invariants shared by internal modules |

## Operator surfaces

The embedded web console and the standalone terminal console consume the same
Bearer-authenticated operator API. Lists and metrics use `/v1/admin/*`; browser
detail uses the existing `/v1/investigations/{id}`, `/timeline`, and `/messages`
reads. PostgreSQL remains behind the control plane: clients never receive
database credentials and approval decisions always
pass through `approvals.Service`, which owns expiry, concurrency, actor, and
audit invariants.

The admin read model summarizes investigation, approval, and durable-job state.
Clients poll this model and tolerate partial refresh failures; `/healthz`
remains a liveness endpoint rather than the source of operational metrics. The
browser UI stores its operator token only for the tab session. The terminal UI
runs as a separate process and receives the token from its environment.

Full reports are detail-only and excluded from polling admin lists. The browser
renders report, citation, argument, policy, and message values as literal text,
never HTML or evidence-derived URL links. Evidence citation metadata comes from
`evidence.collected` events: `evidence_id`, `kind`, `capability`, `observed_at`,
and the connector actor. Legacy metadata may be absent. Proposed actions expose
their exact argument JSON and digest from `action.proposed`; policy is joined
by `action_id` from `policy.evaluated`. Conversation loading can fail without
discarding the report. The terminal console currently loads compact summaries,
not full detail.

## Core domain

Identifiers are distinct named string types (`TicketID`, `InvestigationID`,
`ActionID`, and similar) to prevent accidental interchange. Closed states use
typed constants.

- `Ticket`: normalized input from a ticket connector, including its stable
  external ID and source.
- `Investigation`: one replayable attempt to investigate a ticket, with status,
  prompt/model identity, timestamps, and failure information.
- `CaseMessage`: immutable inbound or outbound requester context. A question
  moves an investigation to `WAITING_ON_REQUESTER`; a deduplicated reply moves
  it back to `PENDING` and creates a new durable job.
- `Evidence`: a structured observation with source, kind, human summary,
  machine-readable data, observation time, and explicit collector capability
  provenance.
- `WorkPackage`: a versioned completion report with cited findings, assumptions,
  unknowns, escalation reason, requester summary, and expected impact.
- `Connector`: an adapter that registers capability metadata and implements
  either safe query operations, privileged operations, or notification paths.
- `Capability`: a stable name, description, read/mutate effect, argument schema
  version, and idempotency metadata.
- `ProposedAction`: an immutable capability invocation proposed by a model. Its
  digest covers a versioned canonical representation of capability and
  arguments.
- `PolicyDecision`: `ALLOW`, `REQUIRE_APPROVAL`, or `DENY`, plus a
  machine-readable reason.
- `Approval`: a decision tied to both `ActionID` and the exact action digest.
- `Execution`: an idempotent attempt by the deterministic executor.
- `Verification`: structured postcondition evidence for an execution.
- `AuditEvent`: an append-only event with investigation-local sequence, actor,
  timestamp, and structured data.

An approval is valid only while its stored action digest equals a freshly
computed digest of the action. Proposed actions are not updated in place; a
changed proposal receives a new ID and approval lifecycle.

## Major interfaces

The initial contracts are intentionally small:

```go
type ModelProvider interface {
    Name() domain.ModelProviderName
    Diagnose(context.Context, agent.Request) (agent.Result, error)
}

type EvidenceCollector interface {
    Name() domain.ConnectorName
    Collect(context.Context, domain.Ticket) ([]domain.Evidence, error)
}

type PolicyEvaluator interface {
    Evaluate(context.Context, domain.ProposedAction) domain.PolicyResult
}

type Sandbox interface {
    Run(context.Context, sandbox.Command) (sandbox.Result, error)
}
```

Persistence is expressed through workflow-level repository operations so a
state change and its audit event can share a database transaction. The agent is
given evidence and read-only tool adapters, never an executor or privileged
connector handle.

## Database model

PostgreSQL stores:

- `tickets`, unique on `(source, external_id)`, for idempotent webhook delivery.
- `investigations`, including status, model identity, prompt version, terminal
  error, and a monotonically increasing `audit_sequence`.
- `evidence`, retaining structured JSON and observation metadata.
- `proposed_actions`, with immutable arguments, action digest, and policy result.
- `approvals`, with action digest, decision, actor, expiry, and decision time.
- `executions`, unique on action and idempotency key.
- `verifications`, linked to an execution.
- `audit_events`, unique on `(investigation_id, sequence)` and append-only by
  application role permissions.
- `jobs`, with available time, lease owner/expiry, attempt count, and dedupe key.
- `case_messages`, an append-only conversation ledger keyed by connector and
  external message ID.
- `message_deliveries`, a transactional requester-message outbox with leases,
  bounded attempts, remote IDs, and delivery state.

The runtime requires schema versions 1 through **12**. Migration 8 adds evidence
capability provenance and a newest-first case/capability index; it leaves legacy
provenance empty rather than guessing from connector or kind. It also adds the
optional `investigations.work_package`, publishable only in the
`RUNNING` → `COMPLETED` transition and immutable once stored. Report, proposals,
policy results, outbound result draft, completion, and audit events commit
together.

Foreign keys preserve the aggregate. JSONB is used for vendor-shaped evidence,
action arguments, and event details; frequently queried lifecycle fields remain
typed columns. The first migration includes constraints that make impossible
states difficult to persist.

An audit append locks its investigation row, increments `audit_sequence`, and
inserts the event in the same transaction as the corresponding domain change.
Consumers order by sequence, not wall-clock time.

## Event lifecycle

The first workflow emits this ordered lifecycle:

1. `ticket.received`
2. `dispatcher.routed`, recording classification, specialist, source, rule or
   fallback code, and model identity/confidence when available
3. `investigation.started` and `specialist.started`
4. One `tool.invoked` for each profile-permitted evidence query
5. `evidence.collected` or `evidence.collection_failed` for each result
6. Either `specialist.handed_off` and a new durable job, or `diagnosis.generated`
7. Zero or more `action.proposed`
8. One `policy.evaluated` per action
9. `specialist.completed` and `investigation.completed`, or `investigation.failed`

Future approval/execution stages add `approval.requested`,
`approval.granted|denied|expired`, `action.executed|failed`,
`verification.completed`, and `ticket.updated` without changing the
investigation contract.

### Requester conversation lifecycle

The model may return one concise `follow_up_question` instead of a diagnosis
when a requester can supply the missing fact. In one transaction Wardstone
stores the outbound message, moves the investigation from `RUNNING` to
`WAITING_ON_REQUESTER`, and appends `requester.question_asked`. A connector
delivers a later reply with a stable comment ID. In one transaction Wardstone
deduplicates that ID, stores the inbound message, moves the investigation to
`PENDING`, appends `requester.reply_received` and `investigation.resumed`, and
enqueues a distinct resume job. The next run receives a bounded selection from
the conversation ledger as untrusted model context.

The transactional requester delivery outbox, leased dispatcher, and Jira comment
adapter are implemented. A send-enabled transition can enqueue delivery in the
same transaction as the message; external I/O occurs later in the dispatcher,
with bounded retries for explicitly retryable failures and delivered/failed
audit events. SHADOW stores drafts, does not enqueue deliveries, and does not
start the dispatcher. REVIEW starts a dispatcher that claims only messages
with immutable operator reviews. Slack delivery remains future work.

Events are facts, not commands. Persisting an event does not implicitly publish
it to a process-local channel. Workers query durable jobs and API clients query
timelines directly. The requester-message outbox is distinct from a general
external audit-event publication mechanism.

### Bounded case memory and reports

Every resumed or handed-off run recollects allowed evidence, persists it, then
reloads observations from the same investigation. Repository filtering applies
the active specialist's allowed collector capabilities **before** the newest-first
limit (observation time, then evidence ID). Empty/legacy provenance is hidden
from model memory. The service additionally requires a nonempty ID, the same
investigation, an allowed registered **read** capability, and a connector source
matching that capability. A narrower specialist never receives another role's
restricted observations just because they are in the same case.

Current-run observations have priority over historical memory, with at most
**128 observations** in model input. Diagnosis input has a **256 KiB** budget
(serialized request plus system instructions); orchestration reserves 8 KiB for
framing/instructions and additional room for warnings. Whole observations are
omitted rather than truncating their contents. Conversation prioritizes the
newest 64 messages within 64 KiB and is emitted chronologically. Handoff history
is limited to 16 valid profile-approved transitions; reasons from broader
evidence scopes are withheld. Up to 32 collection-gap capabilities are retained
only when allowed by the current role, without raw historical connector errors.
Omission and stale/history warnings accompany model context. With no allowed
observations, the model must ask for a missing fact or explain uncertainty and
propose no action.

Reports and actions may cite **only IDs actually supplied to that model run**.
Stored but filtered or budget-omitted observations cannot be cited. All action
proposals require a nonblank reason and 1–128 unique supplied evidence IDs.
For a **mutation**, every cited observation must also have been collected in
the current run, have a nonzero timestamp no older than **15 minutes**, and be
no more than **one minute in the future**. These checks run when building the
input and again after the model returns; evidence can age out during the model
call. A recent prior-run observation is still historical and cannot ground a
mutation. Stale or historical evidence may support report context or read
proposals, but does not grant authority. Capability/profile validation and policy
evaluation still follow citation validation; SHADOW denies all mutations with
`shadow_mode` and executes no proposed reads.

The detail response optionally includes `work_package` with this version-1
schema:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `summary` | Required nonblank operator summary |
| `findings` | Findings with `kind` (`verified` or `assumption`), nonblank `statement`, and `evidence_ids` |
| `unknowns` | Nonblank outstanding questions or gaps |
| `escalation_reason` | Why operator judgment/authority is needed; empty if none |
| `requester_summary` | Requester-facing draft, separate from internal findings |
| `expected_impact` | Expected outcome/impact of proposed next steps |
| `risk_assessment` | Optional provisional vendor rating, rationale, evidence IDs, and Go scoring snapshot |

Minimal example (the citation must be a real supplied ID):

```json
{
  "version": 1,
  "summary": "Direct group membership is missing; operator review is needed.",
  "findings": [
    {
      "kind": "verified",
      "statement": "The resolved user is not a direct member of the mapped group.",
      "evidence_ids": ["evidence-id-from-this-case"]
    }
  ],
  "unknowns": ["Whether access is approved for this user"],
  "escalation_reason": "Business authorization is not established.",
  "requester_summary": "Your request is ready for operator review.",
  "expected_impact": "No changes have been made."
}
```

Verified findings require citations; assumptions may have none. Reports are
limited to 64 KiB, 32 findings, 32 unknowns, 32 citations per finding, and 128
citations overall. Duplicate statements, unknowns, or IDs within a finding,
unsupported versions/kinds, unknown/case-variant/duplicate JSON fields, null
fields, invalid text, and unsupplied citations are rejected. The current
OpenAI-compatible provider requires a work package for a final diagnosis;
follow-up questions and handoffs cannot include a report, diagnosis, or actions.
Legacy cases may retain only a diagnosis. A missing diagnosis is filled from the
report summary. The outbound final draft uses `requester_summary` (or a generic
review message when it is empty), not the internal report summary, and appends
proposal/policy information.

## Concurrency model

Each investigation is a structured unit of concurrency:

- The worker owns the root context and cancels all child calls on shutdown,
  lease loss, or investigation cancellation.
- Evidence collectors are independent I/O operations and run concurrently
  after identity resolution.
- A per-investigation semaphore bounds collector concurrency; a process-level
  worker count bounds active investigations.
- Every collector receives a child context with a configured timeout.
- Each goroutine writes exactly one indexed result and exits. The parent waits
  for all children or cancellation; there are no detached goroutines.
- Results are aggregated in collector registration order, producing stable
  model input and audit processing independent of completion timing.
- Partial collector failures are recorded and passed to the model as warnings.
  Cancellation stops the run; loss of allowed evidence produces explicit
  uncertainty context so the model can ask a question or return a no-action report.
- Slow model calls occupy only their investigation worker, not the HTTP server
  or unrelated workers.

Channels are used only for bounded work coordination where ownership is clear.
Repository state, approvals, and jobs are never represented only by a channel.

### Operations that may run concurrently

- Independent Google user, group, device, Jira-history, documentation, related
  ticket, and vendor-documentation reads after required identity resolution.
- Separate investigations, up to the configured worker bound.
- Independent read-only verification queries after an execution when none
  depends on another result.

### Operations that remain ordered

- Normalize/deduplicate ticket before creating an investigation.
- Resolve identity before queries that require the resolved principal.
- Persist evidence before asking the model to reason over it.
- Validate and persist a proposal before policy evaluation.
- Evaluate policy before requesting approval or executing.
- Validate approval digest and expiry before execution.
- Execute before verifying; verify before updating Jira.
- Persist the result of each state transition with its audit event.

## Trust boundaries and credentials

The HTTP boundary authenticates webhook and operator requests before normalized
domain data enters the core. Vendor payloads, model output, ticket content, and
sandbox output are untrusted input and are validated with size and shape limits.

The agent/model boundary receives ticket text, selected evidence, and the
administrator-configured profile for its selected specialist. The dispatcher
has no connector or mutation capability. A profile can see only its allowlisted
read collectors and can propose only its allowlisted capabilities; both are
validated by deterministic Go code. A model may request an installed,
profile-approved handoff, but it cannot select arbitrary tools, capabilities,
or authority. It has no reference to `executor.Executor`, approval storage, or
privileged mutators. Prompt text can never grant authority.

The executor boundary owns privileged connector interfaces. It independently
loads the immutable action, capability metadata, current policy result, and
approval; checks mode, digest, expiry, and idempotency; then invokes exactly the
registered operation. Connector credentials exist only in connector adapter
configuration or an external secret source and are never placed in model
prompts, audit payloads, job payloads, or sandbox environment variables.

The sandbox runs with an explicit environment allowlist, isolated filesystem
and network policy, resource limits, and no inherited connector credentials.
The initial local implementation is development-only; Docker isolation is the
production-oriented baseline.

PostgreSQL contains durable control-plane state. Migrations run as an owner role;
the application uses a restricted runtime role that cannot update or delete
audit events. Remote deployments should also use TLS, encrypted storage, and
backups. Sensitive vendor payloads are stored as received; configurable
redaction is planned and must not be assumed.

## SHADOW vertical slice

1. A Jira webhook is authenticated and normalized.
2. The ticket and pending investigation/job are inserted idempotently.
3. A bounded worker claims the job and marks the investigation running.
4. Ordered rules first route from Jira metadata. If no rule matches, a bounded
   intent classifier can choose an installed specialist at the configured
   confidence threshold. Failure or uncertainty assigns Help Desk. The original
   decision is immutable and persisted with its audit event. Only the chosen
   profile's allowed read collectors gather context concurrently.
5. Successful evidence and structured failures are persisted.
6. The configured model provider receives bounded, provenance-filtered case
   memory and generates a structured report and zero or more action proposals
   referencing supplied evidence IDs, or asks a question/requests a handoff.
7. Capability names and argument JSON are validated, action digests are
   computed, and proposals are persisted immutably.
8. Policy evaluates each proposal. SHADOW mode forces every mutating action to
   `DENY` with reason `shadow_mode`; read capabilities may be allowed but are not
   executed from model proposals in this slice.
9. The investigation completes and its ordered timeline plus outbound drafts are
   available by API. SHADOW mode never sends requester messages or performs
   connector writes.

No Slack approval is requested and no Google mutation is possible in this
milestone. Approval and executor contracts/schema exist to make the later path
explicit, not active.

### Setup and connector readiness

PostgreSQL, the policy file, and the operator token form the bootstrap control
plane. Jira, Google Workspace, and the model provider are optional connector
groups; partial credential groups are rejected rather than silently ignored.
Without a model the HTTP/admin control plane remains available, but intake is
disabled and workers do not claim pending jobs.

The authenticated setup API returns configuration presence, enabled runtime
features, and sanitized cached probe outcomes. A status read performs no
external I/O. Operators explicitly start a bounded read-only probe for one
fixed connector name; arbitrary URLs, credentials, and probe arguments are not
accepted from the browser. Probes reject redirects, bound response bodies,
validate response shape, suppress upstream bodies/errors, prevent concurrent
duplicates, and apply a short per-connector cooldown. Secrets remain in process
configuration and are never returned through the API. Each completed probe logs
only its fixed component name, stable result code, and duration.

## Failure, cancellation, timeout, and retry design

### Reviewed Jira delivery

Migration 9 adds immutable review text, actor, time, and original draft SHA-256
to `message_deliveries`. REVIEW never automatically enqueues model output. An
operator-authenticated review transaction locks the case, checks source,
direction, draft digest, current state, and latest message, then inserts the
exact reviewed text and its audit event. Identical concurrent reviews return
the same delivery; a changed review conflicts. Original case messages remain
immutable. Resumed reasoning uses approved text rather than superseded drafts.

The REVIEW dispatcher excludes legacy unreviewed rows and suppresses drafts
already superseded at claim time. Jira POSTs are not exactly-once operations:
timeouts, malformed acknowledgements, server failures, and expired running
leases can have unknown remote outcomes. These stop as `NEEDS_RECONCILIATION`; known rejected writes stop as `FAILED`.
Only an explicit throttle response is retried automatically. Queued messages
superseded before dispatch become `SUPERSEDED`. Pending deliveries remain durable
across restarts.

Migration 10 replaces legacy `DEAD` states conservatively, adds a monotonically
increasing delivery revision, and records whether a claimed send requires a
message-correlation property. New Jira comments carry the immutable message ID
and approved-text digest as a property. A separate recovery service performs
bounded Jira identity/comment reads outside database transactions. Confirmation
requires an issue-scoped comment with matching author, creation time, exact ADF
body, and required correlation. Legacy sends allow absent correlation; present
but mismatched correlation fails. Negative or unavailable reads never establish
non-delivery.

The operator may positively reconcile a past uncertain/failed send or explicitly
requeue its unchanged approved text. Both actions require the current delivery
revision, actor, and reason; resend also requires duplicate-risk acknowledgement.
Every recorded check advances the revision. A stale verification cannot overwrite
a concurrent resend, and concurrent resends queue once. Resends require a current
waiting/completed case and latest outbound message, rechecked at dispatch. A
historical send can still be verified after a requester reply.

Delivery mutations lock the case before modifying the outbox, matching review and
requester-reply lock order. Claims reload delivery and conversation state under
that lock so a stale join snapshot cannot bypass supersession checks. Short
transactions append send-attempt and recovery events with the associated state
change. Attempts retain their monotonic count; automatic retry limits are not
reset by manual recovery. Lost recovery responses are resolved by reloading the
revision and timeline, rather than repeating a request with a new revision.
See [operator recovery and API examples](development.md#jira-delivery-recovery).

### Bounded specialist research

The research connector registers separate vendor and knowledge read capabilities.
Each selects from an administrator-owned catalog using an exact structured field
or a whole authenticated requester reply. Neither ticket URLs nor model output
can choose fetch targets. Both catalogs permit public HTTPS text and configured
PDF/Markdown/plain-text files resolved relative to the policy file. Confined local
reads reject escaping symlinks and nonregular files. PDF parsing runs in mandatory
Linux namespace isolation without network, server environment, or source-path
access, with deadlines and process/output budgets. Each readable page is a
separate observation tied to the complete original file hash. Missing text pages,
parser failures, and exceeded budgets remain explicit gaps; no OCR is inferred. Public HTTP uses no credentials, proxy, or redirects and checks
resolved addresses at the actual dial boundary. Documents have count, download,
and excerpt budgets; unavailable sources remain explicit gaps.

Observations retain title, configured URL, content hash, claim type, truncation,
and observation time. The audit timeline omits document bodies and local paths.
Vendor claims are evidence of what a vendor states, not independent attestations.
New vendor reports use a versioned administrator-owned rubric and deployment
context. The provider returns only bounded criterion assessments; the runtime
rejects model-generated overall risk assessments. Go validates citations against
supplied matching-vendor research documents, checks current-run source coverage,
and derives the rating. Missing scope, unknown criteria, failed/truncated/omitted
sources, historical evidence, or missing required independent reports produce
UNKNOWN with no score. Complete reviews use weighted unmet/partial criteria and
administrator thresholds. No policy is inferred from ticket text.

Reports store the rubric snapshot/digest, context, model interpretations, and
gaps under `risk_assessment.scoring`; a SYSTEM `vendor.risk_scored` event commits
with completion. Context and citation authority are frozen before invoking the
provider. Vendor-claim findings are labelled separately and Go generates the
requester-summary rating text. Narrow terminology checks catch known pilot
wording failures without claiming to establish semantic truth. Legacy model
ratings remain readable with no retroactive reinterpretation. See
[vendor scoring configuration and evaluation](development.md#vendor-risk-scoring).

Current-run selector observations constrain question/report boundaries. Known
operator configuration errors produce final escalation reports; unresolved
research selectors request an exact name; resolved research selectors produce
reports with remaining gaps as unknowns. Historical observations cannot impose
these current-run constraints. Handoff descriptions cover only allowed targets.

OpenRouter uses a strict schema with disjoint nested question, handoff, and report
variants. Registered capability argument schemas constrain object keys in that provider
wire schema. Capabilities without schema metadata use a JSON-string argument
fallback. Both decode to the ordinary action contract before validation.
Other compatible providers retain JSON-object mode unless schema output is
explicitly enabled. Every provider still passes citation, capability, argument,
handoff, freshness, policy, and report checks. Output limits are 512 tokens for
classification and 4096 for diagnosis; responses truncated at the limit fail.

Mapped access requests also receive a local selector/configuration observation
when Google is unconfigured. It records unknown membership and an operator gap,
and performs no Directory calls. It does not invent observations or permissions.

### Recovery behavior

| Boundary | Behavior |
| --- | --- |
| Jira duplicate delivery | Unique source/external ID and dedupe job key return the existing ticket/investigation |
| Jira duplicate reply | Unique connector/comment ID returns the existing case without a second resume job |
| Untrusted reply author | The author must equal the ticket reporter; bot and third-party comments are rejected |
| Reply races with a duplicate webhook | The ticket investigation row is locked; exactly one transaction records the reply and enqueues resume |
| HTTP client disconnect | Does not cancel accepted durable work; only request parsing/response work uses that context |
| Worker shutdown | Cancels owned investigations, releases/lets leases expire, and leaves retryable durable jobs; jobs stop after three attempts |
| Connector timeout/throttle | Per-call timeout; bounded exponential backoff only for safe reads and explicit retryable responses |
| Partial evidence failure | Persist structured failure, continue when enough evidence remains |
| Model timeout | Fail the attempt without retrying model output in place; an operator may start a new investigation |
| Malformed model output | Reject before persistence as an action; record a redacted validation failure |
| Approval arrives late | Transactionally reject when expired or action digest differs |
| Duplicate execution | Unique action/idempotency key returns the recorded result without invoking the connector again |
| Process restart | Expired job leases become claimable; persisted investigation state determines resume/restart behavior |
| Audit write failure | The associated state transition rolls back |

Retries are operation-specific. Safe Google reads retry bounded transient
responses within their deadline. Whole investigations are not automatically
retried in the first slice because completed workflow stages are not yet keyed
for replay-safe deduplication. Mutations default to no retry. A mutating capability may opt in only if its
connector supplies a stable idempotency key and documents vendor semantics.
Verification is independently retryable because it is read-only.

Cancellation points exist before and after each external call, while waiting
for concurrency capacity, before model invocation, before every transaction,
and before any future execution. Database transactions are short and never wrap
network I/O.

## Implementation sequence

1. Domain types, capability registry, action digest, policy evaluator, and unit tests.
2. Audit/event contracts and in-memory deterministic repositories for tests.
3. Investigation orchestrator with bounded concurrent collectors and a fake model.
4. PostgreSQL schema and repository with transactional state/event methods.
5. Durable worker leasing and Jira ingestion/timeline HTTP endpoints.
6. Google read adapter and one configured model provider; retain fakes for tests.
7. Approval state machine (implemented and inactive in SHADOW); Slack approval adapter (pending).
8. Privileged executor, idempotency, and verification before enabling APPROVAL.
9. Docker sandbox, replay tooling, metrics, traces, and profiling baselines.
10. APPROVAL and AUTONOMOUS modes only after adversarial and recovery testing.

## Observability and performance

Logs use stable IDs and never log credentials or raw secrets. Metrics and traces
are planned; they are not implemented in this milestone.

The service uses ordinary Go profiling endpoints only when explicitly enabled
on a separate administrative listener. Concurrency-sensitive packages run under
`go test -race`. Benchmarks should target policy evaluation, capability lookup,
action digesting, audit serialization, and job claiming; remote API latency is
measured with integration telemetry rather than CPU benchmarks.

### Operator cases and synthetic execution

Migration 11 adds root/parent investigation links and transactional command
receipts. A case is an original investigation with a linear chain of fresh
attempts; IDs and evidence on older attempts remain unchanged. Row locks serialize
cancellation, completion, replies, retry creation, and worker lease takeover.
Workers validate their lease after acquiring the case lock. Cancellation revokes worker
leases and pending deliveries, while already claimed sends retain their actual
outcomes. The queue retires exhausted leases and marks abandoned active attempts
FAILED even when no other job is claimable.

Migration 12 binds the opt-in synthetic execution pilot to an exact approved
action and one execution per action. RUNNING recovery reuses connector receipts;
VERIFYING recovery only reads independently persisted membership/receipt state.
Expired grants cannot start pending work, stale group versions fail, and recovery
is capped at three claims before reconciliation is required. This executor is
installed only with `test.groups.add_member`; it is separate from infrastructure
mode evaluation, which continues to deny mutations in SHADOW and REVIEW.

The evaluation CLI has no store or mutation adapters. It grades bounded saved
final-report cases, using shared `FinalizeReport` validation and Go risk scoring.
A provider generation method exposes parsed responses to the grader before the
normal live-provider citation rejection; live investigations still use the
validated `Diagnose` entry point. Offline fixtures are the default, and live
comparison requires an explicit flag. See the [operator guide](operator-guide.md).
