package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const SchemaVersion int64 = 7

func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := CheckSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

type SchemaMismatchError struct {
	Count, Minimum, Maximum int64
}

func (e SchemaMismatchError) Error() string {
	return fmt.Sprintf("unsupported database schema history count=%d min=%d max=%d (expected versions 1 through %d)", e.Count, e.Minimum, e.Maximum, SchemaVersion)
}

func IsSchemaMismatch(err error) bool {
	var mismatch SchemaMismatchError
	return errors.As(err, &mismatch)
}

func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("database pool is required")
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	var count, minimum, maximum int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(min(version), 0), COALESCE(max(version), 0) FROM schema_migrations`).Scan(&count, &minimum, &maximum); err != nil {
		return fmt.Errorf("verify database schema: %w", err)
	}
	if count != SchemaVersion || minimum != 1 || maximum != SchemaVersion {
		return SchemaMismatchError{Count: count, Minimum: minimum, Maximum: maximum}
	}
	return nil
}
