package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
)

// Lock cases before jobs, just like cancellation and completion. Exhausted
// workers must leave a retryable FAILED attempt, not a permanently RUNNING case.
func recoverExhaustedCases(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT i.id FROM investigations i WHERE i.status IN('PENDING','RUNNING') AND EXISTS(SELECT 1 FROM jobs j WHERE j.investigation_id=i.id AND j.status='RUNNING' AND j.lease_expires_at<clock_timestamp() AND j.attempt>=3) ORDER BY i.id LIMIT 100 FOR UPDATE OF i SKIP LOCKED`)
	if err != nil {
		return err
	}
	ids := []domain.InvestigationID{}
	for rows.Next() {
		var id domain.InvestigationID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		command, err := tx.Exec(ctx, `UPDATE jobs SET status='DEAD',last_error='worker_recovery_limit',completed_at=now(),lease_owner=NULL,lease_expires_at=NULL WHERE investigation_id=$1 AND status='RUNNING' AND lease_expires_at<clock_timestamp() AND attempt>=3`, id)
		if err != nil {
			return err
		}
		if command.RowsAffected() == 0 {
			continue
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE investigation_id=$1 AND status IN('PENDING','RUNNING'))`, id).Scan(&active); err != nil {
			return err
		}
		if active {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE investigations SET status='FAILED',failure='worker_recovery_limit',completed_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, id, audit.InvestigationFailed, audit.ActorSystem, "worker", time.Now().UTC(), map[string]any{"error": "worker_recovery_limit"}); err != nil {
			return err
		}
	}
	// A process can also die after committing its result but before finishing its
	// job. Its terminal/waiting case remains intact while that old job is retired.
	_, err = tx.Exec(ctx, `UPDATE jobs SET status='DEAD',last_error='worker_recovery_limit',completed_at=now(),lease_owner=NULL,lease_expires_at=NULL WHERE status='RUNNING' AND lease_expires_at<clock_timestamp() AND attempt>=3 AND investigation_id IN(SELECT id FROM investigations WHERE status NOT IN('PENDING','RUNNING'))`)
	return err
}
