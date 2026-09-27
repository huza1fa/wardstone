# Development

## Requirements

- Go 1.24 or later
- Docker with Compose

## Local database

```sh
docker compose up -d postgres
```

The Compose service applies SQL files in `migrations/` when creating a fresh
volume. Remove the development volume before changing an already-applied initial
migration.

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
