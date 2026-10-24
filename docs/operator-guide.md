# Operator guide

Wardstone supports one trusted administrator using the existing bearer token.
Names entered in forms attribute decisions in the audit log; they are not
separate authenticated identities. Use the console over localhost or a trusted
HTTPS endpoint and share access only with people trusted to operate the service.

## Inspect the evidence behind a finding

Open an investigation and choose **View source** beneath a vendor or systems
knowledge citation. The source expands beside the finding or rubric criterion,
showing the stored text excerpt, collection time, source authority, content hash,
and PDF page when recorded. It reads the original observation from that exact
attempt, not the current file or website. Changing/removing the configured source
does not change the stored snapshot. Source URLs are displayed as text and never
fetched by inspection. Expand **Source details and provenance** for the timestamp,
collector capability, source URL, extraction method, and hash.

Compare the passage with the model's claim before approving a requester message.
A valid citation does not prove that its passage supports the claim. Vendor
claims are not independent verification; an administrator-designated independent
report does not establish authenticity. Truncation, unreadable/scanned pages, and
missing excerpts remain explicit gaps. The inspector does not add OCR, restore
omitted text, or establish whether evidence was current-run mutation authority.

Inspection is read only and needs no additional credentials, model calls, or
database migration. Only supported research documents/gaps with explicit matching
collector provenance can be opened; Google payloads and legacy observations with
unknown provenance are not exposed. Excerpts load on demand, stay out of dashboard
polling/audit payloads, and are cleared when closing the detail or disconnecting.
Treat the displayed text as untrusted and potentially sensitive. The operator
token grants access to stored research across all cases; this is not per-user
document authorization or configurable redaction.

`GET /v1/investigations/{id}/evidence/{evidenceID}` uses the operator bearer token
and returns an `evidence` object with `id`, `investigation_id`, `capability`, `kind`,
`observed_at`, and a `document` containing the supported source fields. This is a
fixed-field projection, not arbitrary connector JSON, filesystem paths, or an
original-file download. Reads have a five-second deadline, a 256 KiB stored-JSON
ceiling, and a 24 KiB excerpt ceiling. Responses (including errors) use
`Cache-Control: no-store`. Missing, wrong-attempt, unsupported, malformed, and
over-budget observations all return 404; an unavailable inspection service returns
503. A historical attempt can still be inspected using its own ID, but a retry
cannot read its parent's evidence through the retry's URL.

## Start and manage cases

Open **Start a case → Create a console case**. Enter a summary, useful details,
your operator name, and a reason. Request type and configured vendor/knowledge
names use the existing routing policy and source catalogs. Additional routing
fields accept a JSON object of string arrays. For an access case, supply both
`"Access resource": ["Engineering tools"]` and `"Access user": ["person@example.com"]`
using your configured resource and target user. Internal cases do not invent a
requester email or infer a target user from the operator name. Missing/invalid
access selectors require a new correctly configured case; internal replies do
not change the immutable ticket fields. A configured model
and worker are required. Jira and Google Workspace are optional.

Console cases have origin `operator`. Results stay as internal drafts. If the
specialist asks a question, open the case and use **Submit reply and continue**.
The internal conversation principal is `operator`; the audit event records the
actual operator name and reason. Jira requester replies continue through the
existing authenticated webhook.

The **Case and attempts** panel shows all attempts and their statuses. A case's
root ID is its original investigation ID; existing links keep working. Select an
attempt to inspect its original report, evidence, conversation, and audit trail.

- **Cancel investigation** works for pending, running, or waiting investigations.
  It records a terminal CANCELLED state, fences worker writes, and supersedes
  pending unsent deliveries. Workers notice revoked leases within approximately
  one second; an external provider may still finish work already requested.
- Cancellation cannot undo a Jira request that was already claimed/sent. Its
  outcome remains recorded; an expired ambiguous send still needs Jira recovery.
- **Retry as a new attempt** works for a failed/cancelled leaf attempt. It collects
  fresh evidence and uses current configuration. It does not reuse old evidence,
  conversations, approvals, or reviewed drafts. The old attempt stays intact.
- Repeated requests use a stable request ID. Retry unchanged input after a lost
  response; changing input is a new command. Concurrent retry requests create at
  most one child. A case is bounded to 100 attempts.
- After three exhausted worker claims, an abandoned active attempt becomes FAILED
  with `worker_recovery_limit`. Investigate the underlying failure, then retry it.
  Completed/waiting results survive a crash before job acknowledgement.

The list and overview retain historical attempts and failures; a successful retry
preserves its predecessor's failed status rather than rewriting it.

## Operator API

All routes below use the existing operator bearer token. Command bodies require
`request_id` (8–128 letters/digits/underscores/hyphens), `actor`, and `reason`.
Create adds `summary`, optional `description`, and routing `metadata`; replies
add `body`. Preserve a request ID and identical input after an uncertain response.
A changed request with the same ID returns 409.

| Method | Route | Result |
| --- | --- | --- |
| POST | `/v1/admin/cases` | 202 with `investigation_id` and whether it was created. |
| GET | `/v1/admin/investigations/{id}/case` | Ticket, linked attempts, and model-worker availability. |
| POST | `/v1/admin/investigations/{id}/cancel` | Terminal cancellation, or 409 if it already finished. |
| POST | `/v1/admin/investigations/{id}/retry` | 202 with the new linked attempt, or 409 if a child exists. |
| POST | `/v1/admin/investigations/{id}/reply` | 202 for an internal reply to a waiting console case. |
| GET | `/v1/admin/test-executions` | Pilot availability and the latest 100 proposals/execution outcomes. |
| POST | `/v1/admin/test-executions` | Synthetic proposal; adds `group` and `member` to the command. |
| POST | `/v1/admin/test-executions/approvals/{id}/queue` | Queues one execution after granting the existing approval. |
| POST | `/v1/admin/test-executions/{id}/verify` | Queues a read-only recheck of a failed/uncertain execution. |

Creating/resuming attempts returns 503 when the model worker is unavailable;
cancellation and case history remain available. Synthetic write routes return
503 when their pilot is disabled; history remains readable. Approval decisions
continue through the existing `/v1/admin/approvals/{id}/decision` endpoint.

## Exercise approved execution without infrastructure

The synthetic execution pilot is disabled by default. To enable it, configure:

```sh
WARDSTONE_TEST_EXECUTION_ENABLED=true
```

Restart Wardstone, then use **Test approved execution**. This feature works even
without a model or Google/Jira credentials. Leave `WARDSTONE_MODE` in SHADOW or
REVIEW: the pilot does not enable either APPROVAL or AUTONOMOUS infrastructure
execution. Only the fixed `test.groups.add_member` connector is installed in its
executor; it changes synthetic PostgreSQL tables, never a real directory.

1. Propose a group/member pair such as `test-engineering` and `test-alice`.
   Names must start with `test-` and use lowercase letters, digits, `_`, or `-`.
2. Review the exact arguments, expected group version, and action digest in the
   approval queue. Grant or deny with your operator name.
3. Choose **Queue approved test execution**, with your name and reason.
   Approval must still be granted and unexpired at the first execution claim.
4. Refresh to inspect execution and independent verification. The case's audit
   timeline records the approval, each claim, mutation, and verification.

| State | Meaning / next step |
| --- | --- |
| Not queued | Review the approval; queue it explicitly after granting. |
| PENDING | Durable execution queued; the enabled pilot worker will claim it. |
| RUNNING | Mutation claimed. A crash reuses the exact operation receipt/key. |
| VERIFYING | Mutation was acknowledged, or an operator requested a read-only check. Recovery reads state without repeating the mutation. |
| SUCCEEDED | A separate read confirmed membership and a matching operation receipt. |
| FAILED | A precondition changed or verification did not prove the postcondition. Inspect the case and create a fresh proposal if a new mutation is needed. |
| NEEDS_RECONCILIATION | Outcome/verification was unavailable, the grant expired before starting, or three recovery claims were exhausted. Inspect and request a read-only check. |

**Verify again — read only** queues an independent check for failed/uncertain
executions. It never resends a mutation. An old success does not guarantee future
state: verification records what a separate read observed at its recorded time.

A proposal captures the synthetic group's version. If another operation changes
that version before execution, the proposal fails rather than applying stale
intent. Connector state and operation receipts commit separately from execution
acknowledgement; this is what allows crash recovery without duplicate mutations.
The local synthetic connector supports durable idempotency. Real connectors will
need their own explicit unknown-outcome handling before they can be installed.

Setting the flag back to false stops the pilot worker and write endpoints after
restart. Existing history remains visible. No reset or deletion endpoint is
provided. Test state is part of the normal PostgreSQL backup.

## Compare saved cases and models

Run the offline regression baseline without a database or network calls:

```sh
go run ./cmd/wardstone-eval --cases evaluations/pilot.json
```

The seven seed cases cover a complete vendor review, missing independent
assurance, truncated evidence, a scanned PDF gap, missing deployment context,
historical observations, and a page-specific knowledge citation. `fixture` grades
recorded responses; it does not measure live model quality.

To compare provider models, explicitly permit live calls:

```sh
go run ./cmd/wardstone-eval \
  --cases evaluations/pilot.json \
  --models 'provider/model-a,provider/model-b' \
  --live --max-calls 20 --timeout 60s \
  --out /path/outside/repo/model-comparison.json
```

The CLI uses `WARDSTONE_OPENAI_BASE_URL` / `WARDSTONE_OPENAI_API_KEY`, or the existing
OpenRouter-only `WARDSTONE_AI_KEY` shortcut. Use `--base-url` for an explicit
compatible endpoint. OpenRouter selects schema output automatically; other
compatible providers can use `--structured-output` when supported.

Results include model/provider, dataset SHA-256, latency, citation validity,
unique citations, evidence completeness, explicit missing-evidence disclosure,
wording checks, report validity, Go-computed score/rating, gap codes, and failed
expectations. It uses the same report finalization and scoring boundary as live
investigations. Parsed output is graded before the normal provider citation
rejection so invalid citations appear as citation failures, rather than being
hidden in a generic request error. These checks do not prove that a cited passage
supports a claim or that a model interpretation is semantically correct.

An incomplete-evidence case can pass when UNKNOWN and disclosure are expected.
`evidence_complete` reports mechanical coverage/scoring gaps, not an independent
assurance of document completeness. Wording checks are explicit phrase rules and
existing vendor-narrative guards. Human calibration is still required. Token
usage and monetary cost are not currently measured.

Saved datasets use version 1 and contain final-report requests, optional
`current_evidence`, recorded results, and expectations. Copy the seed structure;
keep evidence IDs unique and include the administrator rubric/deployment context
for vendor cases. An explicit empty `current_evidence: []` marks all supplied
observations historical. Required citation IDs and forbidden phrases can be
specified per case. This version intentionally grades final reports, with no
proposed mutations, rather than interactive question/handoff trajectories.

Calls are serial and bounded to at most 100 cases, 200 case/model pairs, a 16 MiB
dataset, and a per-call timeout of at most five minutes. There are no collectors,
webhooks, delivery adapters, approval writes, or execution adapters in the CLI.
Live calls send the saved evidence to the selected provider and may incur cost.
Report files are written atomically with private file permissions. Responses are
omitted unless `--include-responses` is supplied (256 KiB capture limit per result). Keep private datasets and
reports outside Git. Exit codes are 0 for all checks passing, 1 for failed checks
or an interrupted run, and 2 for invalid inputs/configuration.
