#!/usr/bin/env bash
set -euo pipefail
: "${WARDSTONE_TEST_DATABASE_URL:?Set WARDSTONE_TEST_DATABASE_URL to a disposable, migrated PostgreSQL test database}"
go vet ./...
go test -race -count=1 ./...
node --test internal/api/web_app_test.js
go build ./cmd/wardstone ./cmd/wardstone-tui ./cmd/wardstone-eval
go run ./cmd/wardstone-eval --cases evaluations/pilot.json
