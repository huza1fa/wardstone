package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

func lockDeliveryCase(ctx context.Context, tx pgx.Tx, id domain.InvestigationID) (domain.InvestigationStatus, error) {
	var status domain.InvestigationStatus
	err := tx.QueryRow(ctx, `SELECT status FROM investigations WHERE id=$1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = investigations.ErrNotFound
	}
	return status, err
}

func (s *Store) GetRecoveryJob(ctx context.Context, id domain.InvestigationID, mid domain.MessageID) (delivery.Job, error) {
	var job delivery.Job
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT m.id,m.investigation_id,m.source,m.direction,m.body,t.source,t.external_id
   FROM case_messages m JOIN investigations i ON i.id=m.investigation_id JOIN tickets t ON t.id=i.ticket_id
   WHERE m.id=$1 AND m.investigation_id=$2`, mid, id).Scan(&job.Message.ID, &job.Message.InvestigationID, &job.Message.Source, &job.Message.Direction, &job.Message.Body, &job.Ticket.Source, &job.Ticket.ExternalID)
		if err == nil {
			job.Delivery, err = readDelivery(ctx, tx, mid)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return investigations.ErrNotFound
		}
		job.Message.Body = job.Delivery.ReviewedBody
		return err
	})
	return job, err
}

func (s *Store) RecordRecovery(ctx context.Context, job delivery.Job, r delivery.RecoveryRequest, outcome string, at time.Time) (domain.MessageDelivery, error) {
	var result domain.MessageDelivery
	if err := r.Validate(); err != nil {
		return result, err
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		status, err := lockDeliveryCase(ctx, tx, job.Message.InvestigationID)
		if err != nil {
			return err
		}
		current, err := readDelivery(ctx, tx, job.Message.ID)
		if err != nil {
			return err
		}
		if current.ID != job.Delivery.ID || current.Revision != *r.Revision || current.ReviewedAt == nil ||
			current.ReviewedBody != job.Message.Body || (current.Status != domain.DeliveryUncertain && current.Status != domain.DeliveryFailed) {
			return delivery.ErrRecoveryConflict
		}
		event := audit.RequesterDeliveryChecked
		next := current.Status
		remoteID := ""
		var deliveredAt *time.Time
		switch r.Action {
		case "reconcile":
			switch outcome {
			case "verified":
				var used bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM message_deliveries d JOIN case_messages m ON m.id=d.message_id WHERE m.investigation_id=$1 AND d.id<>$2 AND d.status='SENT' AND d.remote_id=$3)`, job.Message.InvestigationID, current.ID, r.RemoteID).Scan(&used); err != nil {
					return err
				}
				if used {
					return delivery.ErrRecoveryConflict
				}
				next = domain.DeliverySent
				remoteID = r.RemoteID
				deliveredAt = &at
				event = audit.RequesterDeliveryReconciled
			case "comment_not_found", "comment_mismatch", "jira_verification_unavailable":
			default:
				return delivery.ErrInvalidRecovery
			}
		case "resend":
			if outcome != "resend_queued" {
				return delivery.ErrInvalidRecovery
			}
			var latest domain.MessageID
			if err := tx.QueryRow(ctx, `SELECT id FROM case_messages WHERE investigation_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, job.Message.InvestigationID).Scan(&latest); err != nil {
				return err
			}
			if latest != job.Message.ID || (status != domain.InvestigationWaiting && status != domain.InvestigationCompleted) {
				return delivery.ErrRecoveryConflict
			}
			next = domain.DeliveryPending
			event = audit.RequesterResendRequested
		}
		_, err = tx.Exec(ctx, `UPDATE message_deliveries SET status=$2,revision=revision+1,remote_id=$3,delivered_at=$4,
   available_at=$5,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, current.ID, next, remoteID, deliveredAt, at)
		if err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, job.Message.InvestigationID, event, audit.ActorOperator, r.Actor, at, map[string]any{
			"message_id": job.Message.ID, "delivery_id": current.ID, "attempt": current.Attempt, "previous_status": current.Status,
			"outcome": outcome, "remote_id": r.RemoteID, "reason": r.Reason, "revision": current.Revision + 1,
			"approved_body_digest": delivery.DraftDigest(current.ReviewedBody), "acknowledge_duplicate_risk": r.AcknowledgeDuplicateRisk,
		}); err != nil {
			return err
		}
		result, err = readDelivery(ctx, tx, job.Message.ID)
		return err
	})
	return result, err
}
