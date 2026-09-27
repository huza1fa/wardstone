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
  < migrations/000002_approval_invariants.sql
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
