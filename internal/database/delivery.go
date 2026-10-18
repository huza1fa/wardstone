package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func (s *Store) ClaimDelivery(ctx context.Context, owner string, lease time.Duration) (delivery.Job, error) {
	return s.claimDelivery(ctx, owner, lease, false)
}

func (s *Store) ClaimReviewedDelivery(ctx context.Context, owner string, lease time.Duration) (delivery.Job, error) {
	return s.claimDelivery(ctx, owner, lease, true)
}

func (s *Store) claimDelivery(ctx context.Context, owner string, lease time.Duration, reviewedOnly bool) (delivery.Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return delivery.Job{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var job delivery.Job
	var caseStatus domain.InvestigationStatus
	var latestMessage domain.MessageID
	err = tx.QueryRow(ctx, `SELECT d.id, d.message_id, d.status, d.attempt, d.remote_id, d.last_error,
		d.available_at, d.created_at, d.revision, d.correlation_required, COALESCE(d.reviewed_body,''), d.reviewed_at, m.id, m.investigation_id, m.source, m.external_id, m.direction,
		m.author, COALESCE(d.reviewed_body, m.body), m.created_at, t.id, t.source, t.external_id, t.summary, t.description,
		t.reporter_email, t.created_at, i.status,
		(SELECT newest.id FROM case_messages newest WHERE newest.investigation_id=i.id ORDER BY newest.created_at DESC,newest.id DESC LIMIT 1)
		FROM message_deliveries d
		JOIN case_messages m ON m.id = d.message_id
		JOIN investigations i ON i.id = m.investigation_id
		JOIN tickets t ON t.id = i.ticket_id
		WHERE (NOT $1 OR d.reviewed_at IS NOT NULL) AND d.available_at <= now() AND (d.status = 'PENDING' OR (d.status = 'RUNNING' AND d.lease_expires_at < now()))
		ORDER BY d.available_at, d.created_at FOR UPDATE OF i SKIP LOCKED LIMIT 1`, reviewedOnly).Scan(
		&job.Delivery.ID, &job.Delivery.MessageID, &job.Delivery.Status, &job.Delivery.Attempt, &job.Delivery.RemoteID, &job.Delivery.LastError,
		&job.Delivery.AvailableAt, &job.Delivery.CreatedAt, &job.Delivery.Revision, &job.Delivery.CorrelationRequired, &job.Delivery.ReviewedBody, &job.Delivery.ReviewedAt, &job.Message.ID, &job.Message.InvestigationID, &job.Message.Source, &job.Message.ExternalID,
		&job.Message.Direction, &job.Message.Author, &job.Message.Body, &job.Message.CreatedAt, &job.Ticket.ID, &job.Ticket.Source,
		&job.Ticket.ExternalID, &job.Ticket.Summary, &job.Ticket.Description, &job.Ticket.ReporterEmail, &job.Ticket.CreatedAt, &caseStatus, &latestMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery.Job{}, delivery.ErrNoDelivery
	}
	if err != nil {
		return delivery.Job{}, err
	}
	// Locking only the case avoids lock-order inversions with replies/recovery.
	// A join may have been read before a concurrent case transaction committed;
	// reload under the case lock before relying on delivery or conversation state.
	current, err := readDelivery(ctx, tx, job.Message.ID)
	if err != nil {
		return delivery.Job{}, err
	}
	if current.Revision != job.Delivery.Revision || current.Status != job.Delivery.Status {
		return delivery.Job{}, delivery.ErrNoDelivery
	}
	if err := tx.QueryRow(ctx, `SELECT status,(SELECT id FROM case_messages WHERE investigation_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1) FROM investigations WHERE id=$1`, job.Message.InvestigationID).Scan(&caseStatus, &latestMessage); err != nil {
		return delivery.Job{}, err
	}
	// An expired reviewed send has an unknown remote outcome. Do not repeat a
	// Jira POST after a crash; leave an auditable reconciliation requirement.
	failure := ""
	failureStatus := domain.DeliveryUncertain
	if job.Delivery.Status == domain.DeliveryRunning && job.Delivery.ReviewedAt != nil {
		failure = "delivery_outcome_unknown"
	}
	if reviewedOnly && job.Delivery.Status == domain.DeliveryPending && (latestMessage != job.Message.ID || (caseStatus != domain.InvestigationWaiting && caseStatus != domain.InvestigationCompleted)) {
		failure = "draft_superseded"
		failureStatus = domain.DeliverySuperseded
	}
	if failure != "" {
		if _, err := tx.Exec(ctx, `UPDATE message_deliveries SET status=$3,last_error=$2,revision=revision+1,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, job.Delivery.ID, failure, failureStatus); err != nil {
			return delivery.Job{}, err
		}
		if err := appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterMessageFailed, audit.ActorSystem, "delivery", time.Now().UTC(), map[string]any{"message_id": job.Message.ID, "delivery_id": job.Delivery.ID, "error": failure, "status": failureStatus, "attempt": job.Delivery.Attempt}); err != nil {
			return delivery.Job{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return delivery.Job{}, err
		}
		return delivery.Job{}, delivery.ErrNoDelivery
	}
	job.Delivery.Attempt++
	job.Delivery.Status = domain.DeliveryRunning
	job.Delivery.Revision++
	job.Delivery.LeaseOwner = owner
	job.Delivery.CorrelationRequired = job.Delivery.ReviewedAt != nil
	_, err = tx.Exec(ctx, `UPDATE message_deliveries SET status = 'RUNNING', correlation_required=$5, revision=revision+1, attempt = $2, lease_owner = $3,
		lease_expires_at = now() + $4::interval WHERE id = $1`, job.Delivery.ID, job.Delivery.Attempt, owner, lease.String(), job.Delivery.CorrelationRequired)
	if err != nil {
		return delivery.Job{}, err
	}
	if err := appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterSendStarted, audit.ActorSystem, "delivery", time.Now().UTC(), map[string]any{"message_id": job.Message.ID, "delivery_id": job.Delivery.ID, "attempt": job.Delivery.Attempt, "approved_body_digest": delivery.DraftDigest(job.Message.Body)}); err != nil {
		return delivery.Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return delivery.Job{}, err
	}
	return job, nil
}

func (s *Store) CompleteDelivery(ctx context.Context, job delivery.Job, remoteID string, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := lockDeliveryCase(ctx, tx, job.Message.InvestigationID); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE message_deliveries SET status = 'SENT', revision=revision+1, remote_id = $2, delivered_at = $3,
			lease_owner = NULL, lease_expires_at = NULL WHERE id = $1 AND status = 'RUNNING' AND lease_owner = $4
			AND attempt = $5 AND lease_expires_at > now()`, job.Delivery.ID, remoteID, at, job.Delivery.LeaseOwner, job.Delivery.Attempt)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return delivery.ErrLeaseLost
		}
		return appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterMessageDelivered, audit.ActorConnector, string(job.Message.Source), at, map[string]any{"message_id": job.Message.ID, "delivery_id": job.Delivery.ID, "remote_id": remoteID, "attempt": job.Delivery.Attempt})
	})
}

func (s *Store) RetryDelivery(ctx context.Context, job delivery.Job, failure string, at, next time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := lockDeliveryCase(ctx, tx, job.Message.InvestigationID); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE message_deliveries SET status='PENDING',revision=revision+1,last_error=$2,available_at=$3,
   lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND status='RUNNING' AND lease_owner=$4 AND attempt=$5 AND lease_expires_at>now()`, job.Delivery.ID, failure, next, job.Delivery.LeaseOwner, job.Delivery.Attempt)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return delivery.ErrLeaseLost
		}
		return appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterSendRetried, audit.ActorSystem, "delivery", at, map[string]any{"delivery_id": job.Delivery.ID, "message_id": job.Message.ID, "attempt": job.Delivery.Attempt, "error": failure})
	})
}

func (s *Store) FailDelivery(ctx context.Context, job delivery.Job, failure string, status domain.DeliveryStatus, at time.Time) error {
	if status != domain.DeliveryFailed && status != domain.DeliveryUncertain {
		return fmt.Errorf("invalid delivery failure status %q", status)
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := lockDeliveryCase(ctx, tx, job.Message.InvestigationID); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE message_deliveries SET status=$5,revision=revision+1,last_error=$2,
   lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND status='RUNNING' AND lease_owner=$3 AND attempt=$4 AND lease_expires_at>now()`, job.Delivery.ID, failure, job.Delivery.LeaseOwner, job.Delivery.Attempt, status)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return delivery.ErrLeaseLost
		}
		return appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterMessageFailed, audit.ActorSystem, "delivery", at, map[string]any{"message_id": job.Message.ID, "delivery_id": job.Delivery.ID, "error": failure, "status": status, "attempt": job.Delivery.Attempt})
	})
}
