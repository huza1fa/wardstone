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
| `internal/worker` | Bounded workers over durably leased jobs |
| `internal/sandbox` | Unprivileged research runtime contract |
| `internal/tui` | Charm terminal client and presentation model over the operator API |
| `internal/domain` | Domain types and invariants shared by internal modules |

## Operator surfaces

The embedded web console and the standalone terminal console consume the same
Bearer-authenticated `/v1/admin/*` API. PostgreSQL remains behind the control
plane: clients never receive database credentials and approval decisions always
pass through `approvals.Service`, which owns expiry, concurrency, actor, and
audit invariants.

The admin read model summarizes investigation, approval, and durable-job state.
Clients poll this model and tolerate partial refresh failures; `/healthz`
remains a liveness endpoint rather than the source of operational metrics. The
browser UI stores its operator token only for the tab session. The terminal UI
runs as a separate process and receives the token from its environment.

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
  machine-readable data, and observation time.
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
2. `investigation.started`
3. One `tool.invoked` for each planned evidence query
4. `evidence.collected` or `evidence.collection_failed` for each result
5. `diagnosis.generated`
6. Zero or more `action.proposed`
7. One `policy.evaluated` per action
8. `investigation.completed` or `investigation.failed`

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
enqueues a distinct resume job. The next run receives the complete conversation
ledger as untrusted model context.

This slice deliberately stops at durable intake and resume. Sending an outbound
question through Jira/Slack will use a transactional outbox and a connector
delivery adapter, rather than making an external HTTP call inside the state
transition.

Events are facts, not commands. Persisting an event does not implicitly publish
it to a process-local channel. A transactional outbox can be added when an
external event consumer exists; until then, workers query durable jobs and API
clients query timelines directly.

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
  Cancellation and loss of all useful evidence fail the investigation.
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

The agent/model boundary receives ticket text and selected evidence. It can
request registered read capabilities and propose structured actions, but has no
reference to `executor.Executor`, approval storage, or privileged mutators.
Prompt text can never grant authority.

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
backups. Sensitive vendor payloads are redacted before audit persistence.

## SHADOW vertical slice

1. A Jira webhook is authenticated and normalized.
2. The ticket and pending investigation/job are inserted idempotently.
3. A bounded worker claims the job and marks the investigation running.
4. Google read collectors gather user and group context concurrently.
5. Successful evidence and structured failures are persisted.
6. The configured model provider generates a diagnosis and zero or more
   structured action proposals referencing evidence IDs.
7. Capability names and argument JSON are validated, action digests are
   computed, and proposals are persisted immutably.
8. Policy evaluates each proposal. SHADOW mode forces every mutating action to
   `DENY` with reason `shadow_mode`; read capabilities may be allowed but are not
   executed from model proposals in this slice.
9. The investigation completes and its ordered timeline is available by API.

No Slack approval is requested and no Google mutation is possible in this
milestone. Approval and executor contracts/schema exist to make the later path
explicit, not active.

## Failure, cancellation, timeout, and retry design

| Boundary | Behavior |
| --- | --- |
| Jira duplicate delivery | Unique source/external ID and dedupe job key return the existing ticket/investigation |
| Jira duplicate reply | Unique connector/comment ID returns the existing case without a second resume job |
| Reply races with a duplicate webhook | The ticket investigation row is locked; exactly one transaction records the reply and enqueues resume |
| HTTP client disconnect | Does not cancel accepted durable work; only request parsing/response work uses that context |
| Worker shutdown | Cancels owned investigations, releases/lets leases expire, and leaves retryable durable jobs |
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

Logs use stable IDs and never log credentials or raw secrets. Metrics cover job
depth/age, active investigations, collector/model latency, timeout and error
counts, policy decisions, and event append failures. Trace spans follow a ticket
through collectors and the model call.

The service uses ordinary Go profiling endpoints only when explicitly enabled
on a separate administrative listener. Concurrency-sensitive packages run under
`go test -race`. Benchmarks should target policy evaluation, capability lookup,
action digesting, audit serialization, and job claiming; remote API latency is
measured with integration telemetry rather than CPU benchmarks.
