package database

import (
	"context"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func (s *Store) Overview(ctx context.Context) (admin.Overview, error) {
	var result admin.Overview
	if err := s.pool.QueryRow(ctx, `SELECT now(), count(*),
		count(*) FILTER (WHERE status = 'PENDING'),
		count(*) FILTER (WHERE status = 'RUNNING'),
		count(*) FILTER (WHERE status = 'COMPLETED'),
		count(*) FILTER (WHERE status = 'FAILED'),
		count(*) FILTER (WHERE status = 'CANCELLED') FROM investigations`).Scan(
		&result.GeneratedAt, &result.Investigations.Total, &result.Investigations.Pending,
		&result.Investigations.Running, &result.Investigations.Completed,
		&result.Investigations.Failed, &result.Investigations.Cancelled); err != nil {
		return admin.Overview{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE status = 'PENDING'),
		count(*) FILTER (WHERE status = 'GRANTED'),
		count(*) FILTER (WHERE status = 'DENIED'),
		count(*) FILTER (WHERE status = 'EXPIRED') FROM approvals`).Scan(
		&result.Approvals.Total, &result.Approvals.Pending, &result.Approvals.Granted,
		&result.Approvals.Denied, &result.Approvals.Expired); err != nil {
		return admin.Overview{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status = 'PENDING'),
		count(*) FILTER (WHERE status = 'RUNNING'),
		count(*) FILTER (WHERE status = 'COMPLETED'),
		count(*) FILTER (WHERE status = 'DEAD') FROM jobs`).Scan(
		&result.Jobs.Pending, &result.Jobs.Running, &result.Jobs.Completed, &result.Jobs.Dead); err != nil {
		return admin.Overview{}, err
	}
	return result, nil
}

func (s *Store) ListInvestigations(ctx context.Context, limit int) ([]admin.InvestigationSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.id, i.ticket_id, i.status, i.model_provider, i.model,
		i.prompt_version, i.specialist, i.routing, i.diagnosis, i.failure, i.created_at, i.started_at, i.completed_at,
		t.source, t.external_id, t.summary, t.reporter_email
		FROM investigations i JOIN tickets t ON t.id = i.ticket_id
		ORDER BY i.created_at DESC, i.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]admin.InvestigationSummary, 0)
	for rows.Next() {
		var item admin.InvestigationSummary
		if err := rows.Scan(&item.ID, &item.TicketID, &item.Status, &item.ModelProvider, &item.Model,
			&item.PromptVersion, &item.Specialist, &item.Routing, &item.Diagnosis, &item.Failure, &item.CreatedAt, &item.StartedAt,
			&item.CompletedAt, &item.Ticket.Source, &item.Ticket.ExternalID, &item.Ticket.Summary,
			&item.Ticket.ReporterEmail); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListApprovals(ctx context.Context, status domain.ApprovalStatus) ([]admin.ApprovalSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id, a.action_id, a.action_digest, a.status, a.actor,
		a.expires_at, a.decided_at, p.investigation_id, p.capability, p.arguments, p.reason,
		p.policy_reason, p.created_at, t.source, t.external_id, t.summary, t.reporter_email
		FROM approvals a
		JOIN proposed_actions p ON p.id = a.action_id
		JOIN investigations i ON i.id = p.investigation_id
		JOIN tickets t ON t.id = i.ticket_id
		WHERE ($1 = '' OR a.status = $1)
		ORDER BY CASE WHEN a.status = 'PENDING' THEN 0 ELSE 1 END, p.created_at DESC, a.id DESC
		LIMIT 100`, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]admin.ApprovalSummary, 0)
	for rows.Next() {
		var item admin.ApprovalSummary
		if err := rows.Scan(&item.ID, &item.ActionID, &item.ActionDigest, &item.Status, &item.Actor,
			&item.ExpiresAt, &item.DecidedAt, &item.InvestigationID, &item.Capability,
			&item.Arguments, &item.Reason, &item.PolicyReason, &item.CreatedAt, &item.Ticket.Source,
			&item.Ticket.ExternalID, &item.Ticket.Summary, &item.Ticket.ReporterEmail); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
