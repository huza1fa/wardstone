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
  < migrations/000004_jira_delivery_outbox.sql
```

Never reapply a migration whose version already appears in
`schema_migrations`.

## Checks

```sh
go test ./...
go test -race ./...
go vet ./...
```

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

Create a dedicated Jira Cloud service account with only **Browse projects** and
**Add comments** for the development project. Create an Atlassian API token for
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

The outgoing connector is an at-least-once delivery system: Wardstone commits
the question/result and a pending delivery before calling Jira. Retryable
network, 429, and 5xx failures back off up to five attempts. Jira does not offer
an idempotency key for comment creation, so an ambiguous network failure after
Jira accepts a request can produce a duplicate visible comment; the durable
audit timeline records every confirmed remote comment ID.

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
