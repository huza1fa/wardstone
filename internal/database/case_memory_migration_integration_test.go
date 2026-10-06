package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestCaseMemoryMigrationPreservesUnknownLegacyProvenance(t *testing.T) {
	databaseURL := os.Getenv("WARDSTONE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WARDSTONE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := fmt.Sprintf("case_memory_upgrade_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = base.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") }()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	entries, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(name string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("../../migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "000008" {
			apply(entry.Name())
		}
	}
	now := time.Now().UTC()
	id, ticketID, evidenceID := domain.NewInvestigationID(), domain.NewTicketID(), domain.NewEvidenceID()
	if _, err := pool.Exec(ctx, `INSERT INTO tickets(id, source, external_id, summary, created_at) VALUES ($1, 'jira', $1, 'legacy access request', $2)`, ticketID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO investigations(id, ticket_id, status, prompt_version, diagnosis, created_at)
		VALUES ($1, $2, 'COMPLETED', 'legacy', 'Legacy diagnosis remains readable', $3)`, id, ticketID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO evidence(id, investigation_id, source, kind, summary, data, observed_at)
		VALUES ($1, $2, 'google', 'group_membership', 'Legacy observation', '{"member":true}', $3)`, evidenceID, id, now); err != nil {
		t.Fatal(err)
	}
	apply("000008_case_memory_reports.sql")
	if err := CheckSchema(ctx, pool); err != nil {
		t.Fatalf("upgraded schema: %v", err)
	}
	var capability string
	if err := pool.QueryRow(ctx, `SELECT capability FROM evidence WHERE id = $1`, evidenceID).Scan(&capability); err != nil || capability != "" {
		t.Fatalf("migration invented legacy provenance = %q err=%v", capability, err)
	}
	store := NewStore(pool)
	got, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"", "google.groups.read"}, 128)
	if err != nil || len(got) != 0 {
		t.Fatalf("migration disclosed unstamped observations: %+v err=%v", got, err)
	}
	item, err := store.GetInvestigation(ctx, id)
	if err != nil || item.WorkPackage != nil || item.Diagnosis != "Legacy diagnosis remains readable" {
		t.Fatalf("migration changed legacy detail: %+v err=%v", item, err)
	}
	var indexExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND tablename = 'evidence'
		AND indexname = 'evidence_case_capability_newest_idx')`, schema).Scan(&indexExists); err != nil || !indexExists {
		t.Fatalf("evidence provenance/newest index missing: exists=%v err=%v", indexExists, err)
	}
}
