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

func (s *Store) ReviewMessage(ctx context.Context, id domain.InvestigationID, messageID domain.MessageID, review delivery.Review, at time.Time) (domain.MessageDelivery, bool, error) {
	if err := review.Validate(); err != nil {
		return domain.MessageDelivery{}, false, err
	}
	var result domain.MessageDelivery
	created := false
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		// Serialize review with requester resume and completion on the case row.
		var status domain.InvestigationStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM investigations WHERE id=$1 FOR UPDATE`, id).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
			return investigations.ErrNotFound
		} else if err != nil {
			return err
		}
		var draft domain.CaseMessage
		err := tx.QueryRow(ctx, `SELECT id, body, direction, source FROM case_messages WHERE id=$1 AND investigation_id=$2`, messageID, id).Scan(&draft.ID, &draft.Body, &draft.Direction, &draft.Source)
		if errors.Is(err, pgx.ErrNoRows) {
			return investigations.ErrNotFound
		}
		if err != nil {
			return err
		}
		if draft.Direction != domain.MessageOutbound || draft.Source != "jira" || review.DraftDigest != delivery.DraftDigest(draft.Body) {
			return delivery.ErrReviewConflict
		}
		// Idempotent retries return the original review; they never enqueue again.
		result, err = readDelivery(ctx, tx, messageID)
		if err == nil {
			if result.ReviewedBody != review.Body || result.ReviewedBy != review.Actor || result.DraftDigest != review.DraftDigest {
				return delivery.ErrReviewConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if status != domain.InvestigationWaiting && status != domain.InvestigationCompleted {
			return delivery.ErrReviewConflict
		}
		var latest domain.MessageID
		if err := tx.QueryRow(ctx, `SELECT id FROM case_messages WHERE investigation_id=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, id).Scan(&latest); err != nil {
			return err
		}
		if latest != messageID {
			return delivery.ErrReviewConflict
		}
		result = domain.MessageDelivery{ID: domain.NewDeliveryID(), MessageID: messageID, Status: domain.DeliveryPending, AvailableAt: at, CreatedAt: at, ReviewedBody: review.Body, ReviewedBy: review.Actor, ReviewedAt: &at, DraftDigest: review.DraftDigest}
		_, err = tx.Exec(ctx, `INSERT INTO message_deliveries(id,message_id,status,available_at,created_at,reviewed_body,reviewed_by,reviewed_at,draft_digest) VALUES($1,$2,'PENDING',$3,$3,$4,$5,$3,$6)`, result.ID, messageID, at, review.Body, review.Actor, review.DraftDigest)
		if err != nil {
			return err
		}
		created = true
		return appendEvent(ctx, tx, id, audit.RequesterMessageReviewed, audit.ActorOperator, review.Actor, at, map[string]any{"message_id": messageID, "delivery_id": result.ID, "draft_digest": review.DraftDigest, "reviewed_body": review.Body})
	})
	return result, created, err
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readDelivery(ctx context.Context, query rowQuerier, id domain.MessageID) (domain.MessageDelivery, error) {
	var d domain.MessageDelivery
	err := query.QueryRow(ctx, `SELECT id,message_id,status,attempt,remote_id,last_error,available_at,created_at,delivered_at,COALESCE(reviewed_body,''),COALESCE(reviewed_by,''),reviewed_at,COALESCE(draft_digest,''),revision,correlation_required FROM message_deliveries WHERE message_id=$1`, id).Scan(&d.ID, &d.MessageID, &d.Status, &d.Attempt, &d.RemoteID, &d.LastError, &d.AvailableAt, &d.CreatedAt, &d.DeliveredAt, &d.ReviewedBody, &d.ReviewedBy, &d.ReviewedAt, &d.DraftDigest, &d.Revision, &d.CorrelationRequired)
	return d, err
}

func (s *Store) ListDeliveries(ctx context.Context, id domain.InvestigationID) ([]domain.MessageDelivery, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.id,d.message_id,d.status,d.attempt,d.remote_id,d.last_error,d.available_at,d.created_at,d.delivered_at,COALESCE(d.reviewed_body,''),COALESCE(d.reviewed_by,''),d.reviewed_at,COALESCE(d.draft_digest,''),d.revision,d.correlation_required FROM message_deliveries d JOIN case_messages m ON m.id=d.message_id WHERE m.investigation_id=$1 ORDER BY d.created_at,d.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.MessageDelivery{}
	for rows.Next() {
		var d domain.MessageDelivery
		if err := rows.Scan(&d.ID, &d.MessageID, &d.Status, &d.Attempt, &d.RemoteID, &d.LastError, &d.AvailableAt, &d.CreatedAt, &d.DeliveredAt, &d.ReviewedBody, &d.ReviewedBy, &d.ReviewedAt, &d.DraftDigest, &d.Revision, &d.CorrelationRequired); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
