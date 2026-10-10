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
		d.available_at, d.created_at, COALESCE(d.reviewed_body,''), d.reviewed_at, m.id, m.investigation_id, m.source, m.external_id, m.direction,
		m.author, COALESCE(d.reviewed_body, m.body), m.created_at, t.id, t.source, t.external_id, t.summary, t.description,
		t.reporter_email, t.created_at, i.status,
		(SELECT newest.id FROM case_messages newest WHERE newest.investigation_id=i.id ORDER BY newest.created_at DESC,newest.id DESC LIMIT 1)
		FROM message_deliveries d
		JOIN case_messages m ON m.id = d.message_id
		JOIN investigations i ON i.id = m.investigation_id
		JOIN tickets t ON t.id = i.ticket_id
		WHERE (NOT $1 OR d.reviewed_at IS NOT NULL) AND d.available_at <= now() AND (d.status = 'PENDING' OR (d.status = 'RUNNING' AND d.lease_expires_at < now()))
		ORDER BY d.available_at, d.created_at FOR UPDATE OF d SKIP LOCKED LIMIT 1`, reviewedOnly).Scan(
		&job.Delivery.ID, &job.Delivery.MessageID, &job.Delivery.Status, &job.Delivery.Attempt, &job.Delivery.RemoteID, &job.Delivery.LastError,
		&job.Delivery.AvailableAt, &job.Delivery.CreatedAt, &job.Delivery.ReviewedBody, &job.Delivery.ReviewedAt, &job.Message.ID, &job.Message.InvestigationID, &job.Message.Source, &job.Message.ExternalID,
		&job.Message.Direction, &job.Message.Author, &job.Message.Body, &job.Message.CreatedAt, &job.Ticket.ID, &job.Ticket.Source,
		&job.Ticket.ExternalID, &job.Ticket.Summary, &job.Ticket.Description, &job.Ticket.ReporterEmail, &job.Ticket.CreatedAt, &caseStatus, &latestMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery.Job{}, delivery.ErrNoDelivery
	}
	if err != nil {
		return delivery.Job{}, err
	}
	// An expired reviewed send has an unknown remote outcome. Do not repeat a
	// Jira POST after a crash; leave an auditable reconciliation requirement.
	failure := ""
	if job.Delivery.Status == domain.DeliveryRunning && job.Delivery.ReviewedAt != nil {
		failure = "delivery_outcome_unknown"
	}
	if reviewedOnly && job.Delivery.Status == domain.DeliveryPending && (latestMessage != job.Message.ID || (caseStatus != domain.InvestigationWaiting && caseStatus != domain.InvestigationCompleted)) {
		failure = "draft_superseded"
	}
	if failure != "" {
		if _, err := tx.Exec(ctx, `UPDATE message_deliveries SET status='DEAD',last_error=$2,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, job.Delivery.ID, failure); err != nil {
			return delivery.Job{}, err
		}
		if err := appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterMessageFailed, audit.ActorSystem, "delivery", time.Now().UTC(), map[string]any{"message_id": job.Message.ID, "delivery_id": job.Delivery.ID, "error": failure}); err != nil {
			return delivery.Job{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return delivery.Job{}, err
		}
		return delivery.Job{}, delivery.ErrNoDelivery
	}
	job.Delivery.Attempt++
	job.Delivery.Status = domain.DeliveryRunning
	job.Delivery.LeaseOwner = owner
	_, err = tx.Exec(ctx, `UPDATE message_deliveries SET status = 'RUNNING', attempt = $2, lease_owner = $3,
		lease_expires_at = now() + $4::interval WHERE id = $1`, job.Delivery.ID, job.Delivery.Attempt, owner, lease.String())
	if err != nil {
		return delivery.Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return delivery.Job{}, err
	}
	return job, nil
}

func (s *Store) CompleteDelivery(ctx context.Context, job delivery.Job, remoteID string, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE message_deliveries SET status = 'SENT', remote_id = $2, delivered_at = $3,
			lease_owner = NULL, lease_expires_at = NULL WHERE id = $1 AND status = 'RUNNING' AND lease_owner = $4
			AND attempt = $5 AND lease_expires_at > now()`, job.Delivery.ID, remoteID, at, job.Delivery.LeaseOwner, job.Delivery.Attempt)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return delivery.ErrLeaseLost
		}
		return appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterMessageDelivered, audit.ActorConnector, string(job.Message.Source), at, map[string]any{"message_id": job.Message.ID, "delivery_id": job.Delivery.ID, "remote_id": remoteID})
	})
}

func (s *Store) RetryDelivery(ctx context.Context, job delivery.Job, failure string, at, next time.Time) error {
	_ = at
	command, err := s.pool.Exec(ctx, `UPDATE message_deliveries SET status = 'PENDING', last_error = $2, available_at = $3,
		lease_owner = NULL, lease_expires_at = NULL WHERE id = $1 AND status = 'RUNNING' AND lease_owner = $4
		AND attempt = $5 AND lease_expires_at > now()`, job.Delivery.ID, failure, next, job.Delivery.LeaseOwner, job.Delivery.Attempt)
	if err != nil {
		return fmt.Errorf("retry delivery: %w", err)
	}
	if command.RowsAffected() != 1 {
		return delivery.ErrLeaseLost
	}
	return nil
}

func (s *Store) FailDelivery(ctx context.Context, job delivery.Job, failure string, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE message_deliveries SET status = 'DEAD', last_error = $2,
			lease_owner = NULL, lease_expires_at = NULL WHERE id = $1 AND status = 'RUNNING' AND lease_owner = $3
			AND attempt = $4 AND lease_expires_at > now()`, job.Delivery.ID, failure, job.Delivery.LeaseOwner, job.Delivery.Attempt)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return delivery.ErrLeaseLost
		}
		return appendEvent(ctx, tx, job.Message.InvestigationID, audit.RequesterMessageFailed, audit.ActorSystem, "delivery", at, map[string]any{"message_id": job.Message.ID, "delivery_id": job.Delivery.ID, "error": failure})
	})
}
