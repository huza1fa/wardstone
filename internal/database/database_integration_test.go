package database

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCheckSchemaRequiresCompleteHistory(t *testing.T) {
	databaseURL := os.Getenv("WARDSTONE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WARDSTONE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := fmt.Sprintf("readiness_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = base.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	if _, err := base.Exec(ctx, "CREATE TABLE "+quoted+".schema_migrations (version bigint PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
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
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES (1), (7)`); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, pool); !IsSchemaMismatch(err) {
		t.Fatalf("gapped history error = %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	for version := int64(1); version <= SchemaVersion; version++ {
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckSchema(ctx, pool); err != nil {
		t.Fatalf("complete history: %v", err)
	}
}
