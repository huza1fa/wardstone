package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	approvalstate "github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func (s *Store) CreateApproval(ctx context.Context, action domain.ProposedAction, approval domain.Approval, at time.Time) (domain.Approval, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Approval{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var investigationID domain.InvestigationID
	var persistedDigest string
	var decision domain.PolicyDecision
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, action.ID); err != nil {
		return domain.Approval{}, false, fmt.Errorf("lock proposed action for approval: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT investigation_id, action_digest, policy_decision
		FROM proposed_actions WHERE id = $1`, action.ID).Scan(&investigationID, &persistedDigest, &decision)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Approval{}, false, approvalstate.ErrActionNotEligible
	}
	if err != nil {
		return domain.Approval{}, false, fmt.Errorf("load proposed action for approval: %w", err)
	}
	if investigationID != action.InvestigationID || persistedDigest != action.Digest || !action.DigestValid() {
		return domain.Approval{}, false, approvalstate.ErrActionChanged
	}
	if decision != domain.PolicyRequireApproval {
		return domain.Approval{}, false, approvalstate.ErrActionNotEligible
	}
	if approval.ID == "" || approval.ActionID != action.ID || approval.ActionDigest != action.Digest ||
		approval.Status != domain.ApprovalPending || !approval.ExpiresAt.After(at) {
		return domain.Approval{}, false, approvalstate.ErrActionChanged
	}

	existing, err := getActiveApproval(ctx, tx, action.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.Approval{}, false, fmt.Errorf("load active approval: %w", err)
	}
	if err == nil {
		if at.Before(existing.ExpiresAt) {
			if err := tx.Commit(ctx); err != nil {
				return domain.Approval{}, false, err
			}
			return existing, false, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET status = 'EXPIRED' WHERE id = $1`, existing.ID); err != nil {
			return domain.Approval{}, false, fmt.Errorf("expire active approval: %w", err)
		}
		if err := appendEvent(ctx, tx, investigationID, audit.ApprovalExpired, audit.ActorSystem, "approvals", at, map[string]any{
			"approval_id": existing.ID, "action_id": existing.ActionID, "action_digest": existing.ActionDigest,
		}); err != nil {
			return domain.Approval{}, false, err
		}
	}

	_, err = tx.Exec(ctx, `INSERT INTO approvals
		(id, action_id, action_digest, status, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, approval.ID, approval.ActionID, approval.ActionDigest, approval.Status, approval.ExpiresAt)
	if err != nil {
		return domain.Approval{}, false, fmt.Errorf("insert approval: %w", err)
	}
	if err := appendEvent(ctx, tx, investigationID, audit.ApprovalRequested, audit.ActorSystem, "approvals", at, map[string]any{
		"approval_id": approval.ID, "action_id": approval.ActionID, "action_digest": approval.ActionDigest,
		"expires_at": approval.ExpiresAt,
	}); err != nil {
		return domain.Approval{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Approval{}, false, err
	}
	return approval, true, nil
}

func (s *Store) GetApproval(ctx context.Context, id domain.ApprovalID) (domain.Approval, error) {
	var item domain.Approval
	err := s.pool.QueryRow(ctx, `SELECT id, action_id, action_digest, status, actor, expires_at, decided_at
		FROM approvals WHERE id = $1`, id).Scan(&item.ID, &item.ActionID, &item.ActionDigest, &item.Status,
		&item.Actor, &item.ExpiresAt, &item.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Approval{}, approvalstate.ErrNotFound
	}
	return item, err
}

func (s *Store) DecideApproval(ctx context.Context, id domain.ApprovalID, decision domain.ApprovalStatus, actor string, at time.Time) (domain.Approval, error) {
	if err := approvalstate.ValidateDecisionInput(decision, actor); err != nil {
		return domain.Approval{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Approval{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var item domain.Approval
	var investigationID domain.InvestigationID
	err = tx.QueryRow(ctx, `SELECT a.id, a.action_id, a.action_digest, a.status, a.actor,
		a.expires_at, a.decided_at, p.investigation_id
		FROM approvals a JOIN proposed_actions p ON p.id = a.action_id
		WHERE a.id = $1 FOR UPDATE OF a`, id).Scan(&item.ID, &item.ActionID, &item.ActionDigest,
		&item.Status, &item.Actor, &item.ExpiresAt, &item.DecidedAt, &investigationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Approval{}, approvalstate.ErrNotFound
	}
	if err != nil {
		return domain.Approval{}, fmt.Errorf("load approval for decision: %w", err)
	}
	if item.Status == domain.ApprovalExpired {
		return item, approvalstate.ErrExpired
	}
	if item.Status != domain.ApprovalPending {
		return item, approvalstate.ErrAlreadyDecided
	}
	if !at.Before(item.ExpiresAt) {
		item.Status = domain.ApprovalExpired
		if _, err := tx.Exec(ctx, `UPDATE approvals SET status = 'EXPIRED' WHERE id = $1`, id); err != nil {
			return domain.Approval{}, fmt.Errorf("expire approval: %w", err)
		}
		if err := appendEvent(ctx, tx, investigationID, audit.ApprovalExpired, audit.ActorSystem, "approvals", at, map[string]any{
			"approval_id": item.ID, "action_id": item.ActionID, "action_digest": item.ActionDigest,
		}); err != nil {
			return domain.Approval{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Approval{}, err
		}
		return item, approvalstate.ErrExpired
	}

	item.Status = decision
	item.Actor = actor
	item.DecidedAt = &at
	if _, err := tx.Exec(ctx, `UPDATE approvals SET status = $2, actor = $3, decided_at = $4 WHERE id = $1`,
		id, decision, actor, at); err != nil {
		return domain.Approval{}, fmt.Errorf("record approval decision: %w", err)
	}
	eventType := audit.ApprovalGranted
	if decision == domain.ApprovalDenied {
		eventType = audit.ApprovalDenied
	}
	if err := appendEvent(ctx, tx, investigationID, eventType, audit.ActorOperator, actor, at, map[string]any{
		"approval_id": item.ID, "action_id": item.ActionID, "action_digest": item.ActionDigest,
	}); err != nil {
		return domain.Approval{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Approval{}, err
	}
	return item, nil
}

func getActiveApproval(ctx context.Context, tx pgx.Tx, actionID domain.ActionID) (domain.Approval, error) {
	var item domain.Approval
	err := tx.QueryRow(ctx, `SELECT id, action_id, action_digest, status, actor, expires_at, decided_at
		FROM approvals WHERE action_id = $1 AND status IN ('PENDING', 'GRANTED') FOR UPDATE`, actionID).Scan(
		&item.ID, &item.ActionID, &item.ActionDigest, &item.Status, &item.Actor, &item.ExpiresAt, &item.DecidedAt)
	return item, err
}
