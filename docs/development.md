# Development

## Requirements

- Go 1.25 or later
- Docker with Compose

## Local database

```sh
docker compose up -d postgres
```

The Compose service applies SQL files in `migrations/` when creating a fresh
volume. To apply a new migration to an existing development volume, run it
explicitly, for example:

```sh
docker compose exec -T postgres psql -U wardstone_admin -d wardstone \
  < migrations/000007_routing_decisions.sql
```

Never reapply a migration whose version already appears in
`schema_migrations`.

The current runtime requires schema version 7. Apply any missing migrations in
numeric order. Migration 7 preserves old cases without inventing provenance;
new dispatches record their original routing decision.

## Specialist profiles

`WARDSTONE_CONFIG_FILE` is administrator-owned configuration. It defines both
capability policy and specialist profiles. A profile has bounded instructions,
an allowlist of read collectors, an allowlist of capabilities it may propose,
and explicit handoff targets. Follow the supplied configuration when adding a
specialist. A profile never grants execution authority—every proposal still passes deterministic
capability validation and policy.

The example policy includes Help Desk, Access Management, Vendor Review, and
Systems Knowledge profiles. Vendor Review and Systems Knowledge intentionally
have no connector permissions until their bounded, read-only evidence adapters
are configured; defining a profile alone does not create access.

## Checks

```sh
go test ./...
go test -race ./...
go vet ./...
node --test internal/api/web_app_test.js
```

The frontend tests use Node.js 22 or later. Set `WARDSTONE_TEST_DATABASE_URL`
to a migrated PostgreSQL test database to run persistence and API/worker
integration tests. The end-to-end routing test creates and removes its own
schema so it cannot claim unrelated jobs.

The Go process intentionally does not load `.env` files. Export the values from
`.env.example` with your preferred local environment tool before running
`go run ./cmd/wardstone`. Secrets must not be committed. Set
`WARDSTONE_CONFIG_FILE` to the capability policy file; the example environment
uses `examples/wardstone.yaml`.

## Admin web console

Start Wardstone, then open `http://127.0.0.1:8080/admin/` (or the configured
listen address). Enter `WARDSTONE_OPERATOR_TOKEN` when prompted. The token is
kept in browser session storage for the current tab and removed when you
disconnect.

The console shows the operating mode, investigation and job status, recent
diagnoses and timelines, and pending approval requests. Browser and terminal
approval decisions both use the same authenticated API and approval service.
Wardstone is still restricted to SHADOW mode, so the approval queue normally
remains empty in the current milestone.

## Jira connector configuration

Create a dedicated Jira Cloud service account with only **Browse projects** for
the development project. Create an Atlassian API token for
that account and configure the ignored local environment file:

```sh
WARDSTONE_JIRA_BASE_URL=https://your-site.atlassian.net
WARDSTONE_JIRA_EMAIL=wardstone-bot@example.com
WARDSTONE_JIRA_API_TOKEN=replace-me
WARDSTONE_JIRA_WEBHOOK_SECRET=replace-with-a-separate-random-secret
```

The service uses Jira's REST v3 comment endpoint. It requires HTTPS in normal
operation; `http://localhost` and loopback IPs are accepted only for test mocks.
Do not reuse the outbound Jira API token as the inbound webhook secret.

SHADOW mode stores model-generated questions and results as operator-visible
drafts only. It does not call Jira or require **Add comments** permission. The
delivery outbox is reserved for a future operator-reviewed sending workflow.

## Jira ticket routing metadata

Jira Automation should send a small, flattened routing subset with each ticket.
Wardstone evaluates configured `routing.rules` against `issue_type`,
`request_type`, `components`, `labels`, and selected `fields` before it makes
an intent-classification model call. The fields map is intentionally an
allowlisted automation output, not a raw Jira issue payload.

For Jira cascading selects, flatten the selected parent and child into stable
keys such as `customfield_12345.parent` and `customfield_12345.child`. A rule can
require both keys; Wardstone does not interpret arbitrary dotted JSON paths.

Use `match.sources: [jira]` to restrict a rule to Jira tickets. Without a source
selector, the rule can match the metadata supplied by any connector.

```json
{
  "external_id": "HELP-42",
  "summary": "Review Acme's security posture",
  "description": "Procurement needs a recommendation.",
  "reporter_email": "operator@example.com",
  "issue_type": "Service Request",
  "labels": ["vendor"],
  "fields": {"Review type": ["Security"]}
}
```

Rules are ordered: the first match wins. Values within one selector are
alternatives; every selector in a rule is required. Values and source names
ignore surrounding whitespace and letter case. Custom field **keys** ignore
surrounding whitespace but preserve case, so `Review type` and `review type`
identify different fields. Duplicate trimmed field keys and rule names, empty
selectors, unknown YAML options, and oversized rule sets are rejected during
startup. The limits are 128 rules, 64 custom fields per rule, 32 alternatives
per selector, 4,096 selectors in total, and 16,384 alternatives in total.
Ticket summaries and descriptions never trigger hidden keyword routes.

If no structured rule matches, `routing.intent` controls the optional model:

```yaml
routing:
  intent:
    enabled: true
    min_confidence: 0.75
    timeout: 10s
```

These are the defaults when the intent settings are omitted. Set `enabled:
false` to send unmatched tickets directly to Help Desk. Confidence must be
greater than zero and at most one; timeout must be positive and at most five
minutes. The classifier may select only installed specialist profiles. An
unavailable, timed-out, invalid, or low-confidence classification safely stays
with `help_desk`, where the durable follow-up workflow can collect the missing
fact. Routing decisions record whether a rule, model, or fallback selected the
role, together with the rule name or fallback reason and model confidence when
available. A routing decision grants no new permissions: Help Desk and every
specialist retain their configured collectors, proposals, and handoff targets.

Intent classification receives bounded ticket text and selected routing
metadata, omitting the requester identity and case/ticket identifiers. The
encoded request budget is 64 KiB; descriptions are truncated safely at 16 KiB.
An input exceeding that budget records `intent_input_too_large` and assigns
Help Desk. Model redirects are rejected to prevent forwarding ticket data to
another endpoint. Confidence is the model's estimate, not a calibrated
probability or an authorization decision.

The initial decision and `dispatcher.routed` audit event are persisted together
once. Resumes and handoffs do not rerun classification or rewrite its history.
The web and terminal consoles show both initial routing and the current
specialist, including fallback codes, confidence, and classification model.

## Routing preview

Use the web console's Routing preview form or authenticated
`POST /v1/admin/routing/preview` with the same Jira ticket payload. The response
is `{ "routing": { ... } }`. Preview evaluates rules only, stores nothing, and
does not call a model. If no rule matches, it explains whether live intake
would attempt intent classification or assign Help Desk directly.

## Requester replies

When an investigation needs one missing fact, the model stores a durable
outbound question and the investigation moves to `WAITING_ON_REQUESTER`. The
current Jira Automation-friendly reply endpoint is:

```text
POST /v1/tickets/jira/replies
Authorization: Bearer $WARDSTONE_JIRA_WEBHOOK_SECRET
Content-Type: application/json

{"external_id":"HELP-42","comment_id":"jira-comment-id","author":"requester@example.com","body":"The asset tag is LT-1042"}
```

`comment_id` must be stable: repeating the same reply is safe and returns the
existing investigation without creating another resume job. Operators can read
the immutable conversation through
`GET /v1/investigations/{id}/messages` with `WARDSTONE_OPERATOR_TOKEN`.

The `author` must exactly match the ticket `reporter_email`; other commenters,
including the Wardstone bot account, receive `403 Forbidden`. Configure the
Automation rule to exclude the Wardstone service account as a second layer of
protection.

For Jira Automation, map the issue key to `external_id`, the comment's stable
ID to `comment_id`, the author identity to `author`, and the comment text to
`body`. The Automation web request must include:

```text
Authorization: Bearer $WARDSTONE_JIRA_WEBHOOK_SECRET
Content-Type: application/json
```

## Terminal console

Run the Charm-based terminal interface against the same Wardstone process:

```sh
WARDSTONE_OPERATOR_TOKEN=change-me-too go run ./cmd/wardstone-tui
```

`WARDSTONE_API_URL` defaults to `http://127.0.0.1:8080`.
`WARDSTONE_OPERATOR_ACTOR` sets the identity written to approval audit events;
it defaults to the current operating-system user. The equivalent command-line
flags are `-url`, `-actor`, and `-refresh`.

Use `1`/`2`/`3` or `tab` to switch views, arrows or `j`/`k` to move, `r` to
refresh, and `q` to quit. On the approvals view, `a` grants and `d` denies; both
require a final `y` confirmation.
