package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/worker"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) ReceiveTicket(ctx context.Context, ticket domain.Ticket, investigation domain.Investigation) (domain.InvestigationID, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `INSERT INTO tickets
		(id, source, external_id, summary, description, reporter_email, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (source, external_id) DO NOTHING`,
		ticket.ID, ticket.Source, ticket.ExternalID, ticket.Summary, ticket.Description, ticket.ReporterEmail, ticket.CreatedAt)
	if err != nil {
		return "", false, fmt.Errorf("insert ticket: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existing domain.InvestigationID
		err := tx.QueryRow(ctx, `SELECT i.id FROM investigations i JOIN tickets t ON t.id = i.ticket_id
			WHERE t.source = $1 AND t.external_id = $2 ORDER BY i.created_at LIMIT 1`, ticket.Source, ticket.ExternalID).Scan(&existing)
		if err != nil {
			return "", false, fmt.Errorf("find duplicate ticket investigation: %w", err)
		}
		return existing, false, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `INSERT INTO investigations
		(id, ticket_id, status, prompt_version, created_at) VALUES ($1, $2, $3, $4, $5)`,
		investigation.ID, investigation.TicketID, investigation.Status, investigation.PromptVersion, investigation.CreatedAt)
	if err != nil {
		return "", false, fmt.Errorf("insert investigation: %w", err)
	}
	if err := appendEvent(ctx, tx, investigation.ID, audit.TicketReceived, audit.ActorConnector, string(ticket.Source), ticket.CreatedAt, map[string]any{
		"ticket_id": ticket.ID, "source": ticket.Source, "external_id": ticket.ExternalID,
	}); err != nil {
		return "", false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO jobs
		(id, kind, investigation_id, dedupe_key, status, available_at, created_at)
		VALUES ($1, 'investigate', $2, $3, 'PENDING', $4, $4)`,
		domain.NewJobID(), investigation.ID, "investigate:"+string(investigation.ID), investigation.CreatedAt)
	if err != nil {
		return "", false, fmt.Errorf("enqueue investigation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return investigation.ID, true, nil
}

func (s *Store) GetInvestigation(ctx context.Context, id domain.InvestigationID) (domain.Investigation, error) {
	var item domain.Investigation
	err := s.pool.QueryRow(ctx, `SELECT id, ticket_id, status, model_provider, model, prompt_version,
		diagnosis, failure, created_at, started_at, completed_at FROM investigations WHERE id = $1`, id).Scan(
		&item.ID, &item.TicketID, &item.Status, &item.ModelProvider, &item.Model, &item.PromptVersion,
		&item.Diagnosis, &item.Failure, &item.CreatedAt, &item.StartedAt, &item.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Investigation{}, investigations.ErrNotFound
	}
	return item, err
}

func (s *Store) GetTicket(ctx context.Context, id domain.InvestigationID) (domain.Ticket, error) {
	var ticket domain.Ticket
	err := s.pool.QueryRow(ctx, `SELECT t.id, t.source, t.external_id, t.summary, t.description,
		t.reporter_email, t.created_at FROM tickets t JOIN investigations i ON i.ticket_id = t.id WHERE i.id = $1`, id).Scan(
		&ticket.ID, &ticket.Source, &ticket.ExternalID, &ticket.Summary, &ticket.Description, &ticket.ReporterEmail, &ticket.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Ticket{}, investigations.ErrNotFound
	}
	return ticket, err
}

func (s *Store) StartInvestigation(ctx context.Context, id domain.InvestigationID, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := requireLease(ctx, tx, id); err != nil {
			return err
		}
		var promptVersion string
		err := tx.QueryRow(ctx, `UPDATE investigations SET status = 'RUNNING', started_at = COALESCE(started_at, $2)
			WHERE id = $1 AND status IN ('PENDING', 'RUNNING') RETURNING prompt_version`, id, at).Scan(&promptVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return investigations.ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		var starts int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE investigation_id = $1 AND event_type = $2`, id, audit.InvestigationStarted).Scan(&starts); err != nil {
			return err
		}
		if starts == 0 {
			return appendEvent(ctx, tx, id, audit.InvestigationStarted, audit.ActorSystem, "orchestrator", at, map[string]any{"prompt_version": promptVersion})
		}
		return nil
	})
}

func (s *Store) RecordToolInvocations(ctx context.Context, id domain.InvestigationID, tools []investigations.ToolInvocation, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := requireLease(ctx, tx, id); err != nil {
			return err
		}
		if err := requireRunning(ctx, tx, id); err != nil {
			return err
		}
		for _, tool := range tools {
			if err := appendEvent(ctx, tx, id, audit.ToolInvoked, audit.ActorSystem, "orchestrator", at, map[string]any{"connector": tool.Connector, "capability": tool.Capability}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) RecordEvidence(ctx context.Context, id domain.InvestigationID, items []domain.Evidence, failures []investigations.CollectionFailure, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := requireLease(ctx, tx, id); err != nil {
			return err
		}
		if err := requireRunning(ctx, tx, id); err != nil {
			return err
		}
		for _, item := range items {
			_, err := tx.Exec(ctx, `INSERT INTO evidence
				(id, investigation_id, source, kind, summary, data, observed_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				item.ID, id, item.Source, item.Kind, item.Summary, item.Data, item.ObservedAt)
			if err != nil {
				return fmt.Errorf("insert evidence: %w", err)
			}
			if err := appendEvent(ctx, tx, id, audit.EvidenceCollected, audit.ActorConnector, string(item.Source), at, map[string]any{
				"evidence_id": item.ID, "kind": item.Kind, "observed_at": item.ObservedAt,
			}); err != nil {
				return err
			}
		}
		for _, failure := range failures {
			if err := appendEvent(ctx, tx, id, audit.EvidenceCollectionFailed, audit.ActorConnector, string(failure.Source), at, map[string]any{
				"error": failure.Error, "duration_ms": failure.Duration.Milliseconds(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) CompleteInvestigation(ctx context.Context, id domain.InvestigationID, diagnosis string, provider domain.ModelProviderName, model string, evaluations []investigations.ActionEvaluation, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := requireLease(ctx, tx, id); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE investigations SET diagnosis = $2, model_provider = $3,
			model = $4, status = 'COMPLETED', completed_at = $5 WHERE id = $1 AND status = 'RUNNING'`, id, diagnosis, provider, model, at)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return investigations.ErrInvalidTransition
		}
		if err := appendEvent(ctx, tx, id, audit.DiagnosisGenerated, audit.ActorModel, string(provider), at, map[string]any{"model": model}); err != nil {
			return err
		}
		for _, evaluation := range evaluations {
			evidenceIDs, err := json.Marshal(evaluation.Action.EvidenceIDs)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO proposed_actions
				(id, investigation_id, capability, arguments, reason, evidence_ids, action_digest,
				 policy_decision, policy_reason, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
				evaluation.Action.ID, id, evaluation.Action.Capability, evaluation.Action.Arguments,
				evaluation.Action.Reason, evidenceIDs, evaluation.Action.Digest, evaluation.Policy.Decision,
				evaluation.Policy.Reason, evaluation.Action.CreatedAt)
			if err != nil {
				return fmt.Errorf("insert proposed action: %w", err)
			}
			if err := appendEvent(ctx, tx, id, audit.ActionProposed, audit.ActorModel, string(provider), at, map[string]any{
				"action_id": evaluation.Action.ID, "capability": evaluation.Action.Capability,
				"arguments": evaluation.Action.Arguments, "reason": evaluation.Action.Reason,
				"digest": evaluation.Action.Digest, "evidence_ids": evaluation.Action.EvidenceIDs,
			}); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, id, audit.PolicyEvaluated, audit.ActorSystem, "policy", at, map[string]any{
				"action_id": evaluation.Action.ID, "decision": evaluation.Policy.Decision, "reason": evaluation.Policy.Reason,
			}); err != nil {
				return err
			}
		}
		return appendEvent(ctx, tx, id, audit.InvestigationCompleted, audit.ActorSystem, "orchestrator", at, map[string]any{"actions_proposed": len(evaluations)})
	})
}

func (s *Store) FailInvestigation(ctx context.Context, id domain.InvestigationID, failure string, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := requireLease(ctx, tx, id); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE investigations SET status = 'FAILED', failure = $2, completed_at = $3
			WHERE id = $1 AND status IN ('PENDING', 'RUNNING')`, id, failure, at)
		if err != nil || command.RowsAffected() == 0 {
			return err
		}
		return appendEvent(ctx, tx, id, audit.InvestigationFailed, audit.ActorSystem, "orchestrator", at, map[string]any{"error": failure})
	})
}

func (s *Store) Timeline(ctx context.Context, id domain.InvestigationID) ([]audit.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, investigation_id, sequence, event_type, actor_type,
		actor_id, occurred_at, data FROM audit_events WHERE investigation_id = $1 ORDER BY sequence`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []audit.Event
	for rows.Next() {
		var event audit.Event
		if err := rows.Scan(&event.ID, &event.InvestigationID, &event.Sequence, &event.Type, &event.ActorType,
			&event.ActorID, &event.OccurredAt, &event.Data); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		if _, err := s.GetInvestigation(ctx, id); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (s *Store) Claim(ctx context.Context, owner string, lease time.Duration) (worker.Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return worker.Job{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var job worker.Job
	err = tx.QueryRow(ctx, `SELECT id, kind, investigation_id, attempt FROM jobs
		WHERE available_at <= now() AND (status = 'PENDING' OR (status = 'RUNNING' AND lease_expires_at < now()))
		ORDER BY available_at, created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&job.ID, &job.Kind, &job.InvestigationID, &job.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return worker.Job{}, worker.ErrNoJob
	}
	if err != nil {
		return worker.Job{}, err
	}
	job.Attempt++
	job.LeaseOwner = owner
	_, err = tx.Exec(ctx, `UPDATE jobs SET status = 'RUNNING', attempt = $2, lease_owner = $3,
		lease_expires_at = now() + $4::interval WHERE id = $1`, job.ID, job.Attempt, owner, lease.String())
	if err != nil {
		return worker.Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return worker.Job{}, err
	}
	return job, nil
}

func (s *Store) Renew(ctx context.Context, job worker.Job, lease time.Duration) error {
	command, err := s.pool.Exec(ctx, `UPDATE jobs SET lease_expires_at = now() + $4::interval
		WHERE id = $1 AND status = 'RUNNING' AND lease_owner = $2 AND attempt = $3 AND lease_expires_at > now()`,
		job.ID, job.LeaseOwner, job.Attempt, lease.String())
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return worker.ErrLeaseLost
	}
	return nil
}

func (s *Store) Complete(ctx context.Context, job worker.Job) error {
	command, err := s.pool.Exec(ctx, `UPDATE jobs SET status = 'COMPLETED', completed_at = now(),
		lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND status = 'RUNNING' AND lease_owner = $2 AND attempt = $3
		AND lease_expires_at > now()`, job.ID, job.LeaseOwner, job.Attempt)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return worker.ErrLeaseLost
	}
	return nil
}

func (s *Store) Fail(ctx context.Context, job worker.Job, failure string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	command, err := s.pool.Exec(cleanupCtx, `UPDATE jobs SET status = 'DEAD', last_error = $4,
		completed_at = now(), lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND status = 'RUNNING' AND lease_owner = $2 AND attempt = $3
		AND lease_expires_at > now()`, job.ID, job.LeaseOwner, job.Attempt, failure)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return worker.ErrLeaseLost
	}
	return nil
}

func requireLease(ctx context.Context, tx pgx.Tx, investigationID domain.InvestigationID) error {
	job, ok := worker.LeaseFromContext(ctx)
	if !ok {
		return nil
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1 AND investigation_id = $2
		AND status = 'RUNNING' AND lease_owner = $3 AND attempt = $4 AND lease_expires_at > now())`,
		job.ID, investigationID, job.LeaseOwner, job.Attempt).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return worker.ErrLeaseLost
	}
	return nil
}

func requireRunning(ctx context.Context, tx pgx.Tx, id domain.InvestigationID) error {
	var status domain.InvestigationStatus
	if err := tx.QueryRow(ctx, `SELECT status FROM investigations WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
		return err
	}
	if status != domain.InvestigationRunning {
		return investigations.ErrInvalidTransition
	}
	return nil
}

func (s *Store) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appendEvent(ctx context.Context, tx pgx.Tx, id domain.InvestigationID, eventType audit.EventType, actorType audit.ActorType, actorID string, at time.Time, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE investigations SET audit_sequence = audit_sequence + 1
		WHERE id = $1 RETURNING audit_sequence`, id).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate audit sequence: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events
		(investigation_id, sequence, event_type, actor_type, actor_id, occurred_at, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, sequence, eventType, actorType, actorID, at, data)
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}
