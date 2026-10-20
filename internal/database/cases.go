package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

// Command receipt and state transition commit together. The request ID makes
// reconnects safe; reusing it with different input is always a conflict.
func caseCommand(ctx context.Context, tx pgx.Tx, operation, key, digest string) (domain.InvestigationID, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "case:"+operation+":"+key); err != nil {
		return "", err
	}
	var id domain.InvestigationID
	var previous string
	err := tx.QueryRow(ctx, `SELECT investigation_id,digest FROM case_commands WHERE operation=$1 AND request_id=$2`, operation, key).Scan(&id, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if previous != digest {
		return "", cases.ErrConflict
	}
	return id, nil
}
func recordCaseCommand(ctx context.Context, tx pgx.Tx, op string, c cases.Command, digest string, id domain.InvestigationID, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO case_commands(operation,request_id,digest,investigation_id,created_at) VALUES($1,$2,$3,$4,$5)`, op, c.RequestID, digest, id, at)
	return err
}
func enqueueCase(ctx context.Context, tx pgx.Tx, id domain.InvestigationID, key string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO jobs(id,kind,investigation_id,dedupe_key,status,available_at,created_at) VALUES($1,'investigate',$2,$3,'PENDING',$4,$4)`, domain.NewJobID(), id, key, at)
	return err
}
func (s *Store) CreateCase(ctx context.Context, c cases.Create, prompt string, at time.Time) (id domain.InvestigationID, created bool, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	digest := cases.Digest(c)
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		var e error
		id, e = caseCommand(ctx, tx, "create", c.RequestID, digest)
		if e != nil || id != "" {
			return e
		}
		id = domain.NewInvestigationID()
		ticketID := domain.NewTicketID()
		metadata, _ := json.Marshal(c.Metadata)
		if _, e = tx.Exec(ctx, `INSERT INTO tickets(id,source,external_id,summary,description,reporter_email,metadata,created_at) VALUES($1,'operator',$2,$3,$4,'operator',$5,$6)`, ticketID, c.RequestID, c.Summary, c.Description, metadata, at); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO investigations(id,ticket_id,status,prompt_version,created_at) VALUES($1,$2,'PENDING',$3,$4)`, id, ticketID, prompt, at); e != nil {
			return e
		}
		if e = recordCaseCommand(ctx, tx, "create", c.Command, digest, id, at); e != nil {
			return e
		}
		if e = appendEvent(ctx, tx, id, audit.EventType("case.created"), audit.ActorOperator, c.Actor, at, map[string]any{"request_id": c.RequestID, "reason": c.Reason, "ticket_id": ticketID}); e != nil {
			return e
		}
		created = true
		return enqueueCase(ctx, tx, id, "investigate:"+string(id), at)
	})
	return
}
func (s *Store) CancelCase(ctx context.Context, id domain.InvestigationID, c cases.Command, at time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	digest := cases.Digest(struct {
		ID      domain.InvestigationID
		Command cases.Command
	}{id, c})
	return s.transaction(ctx, func(tx pgx.Tx) error {
		previous, err := caseCommand(ctx, tx, "cancel", c.RequestID, digest)
		if err != nil || previous != "" {
			return err
		}
		var status domain.InvestigationStatus
		if err = tx.QueryRow(ctx, `SELECT status FROM investigations WHERE id=$1 FOR UPDATE`, id).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
			return investigations.ErrNotFound
		} else if err != nil {
			return err
		}
		if status != domain.InvestigationPending && status != domain.InvestigationRunning && status != domain.InvestigationWaiting {
			return cases.ErrConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE investigations SET status='CANCELLED',completed_at=$2 WHERE id=$1`, id, at); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE jobs SET status='DEAD',last_error='operator_cancelled',completed_at=$2,lease_owner=NULL,lease_expires_at=NULL WHERE investigation_id=$1 AND status IN ('PENDING','RUNNING')`, id, at); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE approvals SET status='EXPIRED' WHERE action_id IN(SELECT id FROM proposed_actions WHERE investigation_id=$1) AND status IN('PENDING','GRANTED')`, id); err != nil {
			return err
		}
		// A claimed send may already have reached Jira. Retain its lease and outcome
		// rather than pretending cancellation can undo it. Only unsent work stops.
		if _, err = tx.Exec(ctx, `UPDATE message_deliveries SET status='SUPERSEDED',last_error='case_cancelled',revision=revision+1 WHERE message_id IN(SELECT id FROM case_messages WHERE investigation_id=$1) AND status='PENDING'`, id); err != nil {
			return err
		}
		if err = recordCaseCommand(ctx, tx, "cancel", c, digest, id, at); err != nil {
			return err
		}
		return appendEvent(ctx, tx, id, audit.EventType("investigation.cancelled"), audit.ActorOperator, c.Actor, at, map[string]any{"reason": c.Reason, "request_id": c.RequestID})
	})
}
func (s *Store) RetryCase(ctx context.Context, parent domain.InvestigationID, c cases.Command, prompt string, at time.Time) (id domain.InvestigationID, created bool, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	digest := cases.Digest(struct {
		ID      domain.InvestigationID
		Command cases.Command
	}{parent, c})
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		var e error
		id, e = caseCommand(ctx, tx, "retry", c.RequestID, digest)
		if e != nil || id != "" {
			return e
		}
		var root domain.InvestigationID
		var ticket domain.TicketID
		var status domain.InvestigationStatus
		if e = tx.QueryRow(ctx, `SELECT case_id,ticket_id,status FROM investigations WHERE id=$1 FOR UPDATE`, parent).Scan(&root, &ticket, &status); errors.Is(e, pgx.ErrNoRows) {
			return investigations.ErrNotFound
		} else if e != nil {
			return e
		}
		if status != domain.InvestigationFailed && status != domain.InvestigationCancelled {
			return cases.ErrConflict
		}
		var child bool
		var count int
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM investigations WHERE parent_id=$1),(SELECT count(*) FROM investigations WHERE case_id=$2)`, parent, root).Scan(&child, &count); e != nil {
			return e
		}
		if child || count >= 100 {
			return cases.ErrConflict
		}
		id = domain.NewInvestigationID()
		if _, e = tx.Exec(ctx, `INSERT INTO investigations(id,ticket_id,case_id,parent_id,status,prompt_version,created_at) VALUES($1,$2,$3,$4,'PENDING',$5,$6)`, id, ticket, root, parent, prompt, at); e != nil {
			return e
		}
		if e = recordCaseCommand(ctx, tx, "retry", c, digest, id, at); e != nil {
			return e
		}
		data := map[string]any{"parent_id": parent, "investigation_id": id, "case_id": root, "reason": c.Reason, "request_id": c.RequestID, "fresh_evidence": true}
		if e = appendEvent(ctx, tx, parent, audit.EventType("case.retry_created"), audit.ActorOperator, c.Actor, at, data); e != nil {
			return e
		}
		if e = appendEvent(ctx, tx, id, audit.EventType("case.attempt_created"), audit.ActorOperator, c.Actor, at, data); e != nil {
			return e
		}
		created = true
		return enqueueCase(ctx, tx, id, "investigate:"+string(id), at)
	})
	return
}
func (s *Store) ReplyToCase(ctx context.Context, id domain.InvestigationID, r cases.Reply, at time.Time) (created bool, err error) {
	if err = r.Validate(); err != nil {
		return
	}
	digest := cases.Digest(struct {
		ID    domain.InvestigationID
		Reply cases.Reply
	}{id, r})
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		previous, e := caseCommand(ctx, tx, "reply", r.RequestID, digest)
		if e != nil || previous != "" {
			return e
		}
		var status domain.InvestigationStatus
		var source domain.ConnectorName
		e = tx.QueryRow(ctx, `SELECT i.status,t.source FROM investigations i JOIN tickets t ON t.id=i.ticket_id WHERE i.id=$1 FOR UPDATE OF i`, id).Scan(&status, &source)
		if errors.Is(e, pgx.ErrNoRows) {
			return investigations.ErrNotFound
		}
		if e != nil {
			return e
		}
		if status != domain.InvestigationWaiting || source != "operator" {
			return cases.ErrConflict
		}
		messageID := domain.NewMessageID()
		if _, e = tx.Exec(ctx, `INSERT INTO case_messages(id,investigation_id,source,external_id,direction,author,body,created_at) VALUES($1,$2,'operator',$3,'INBOUND','operator',$4,$5)`, messageID, id, r.RequestID, r.Body, at); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE investigations SET status='PENDING' WHERE id=$1`, id); e != nil {
			return e
		}
		if e = recordCaseCommand(ctx, tx, "reply", r.Command, digest, id, at); e != nil {
			return e
		}
		if e = appendEvent(ctx, tx, id, audit.RequesterReplyReceived, audit.ActorOperator, r.Actor, at, map[string]any{"message_id": messageID, "reason": r.Reason}); e != nil {
			return e
		}
		if e = appendEvent(ctx, tx, id, audit.InvestigationResumed, audit.ActorOperator, r.Actor, at, map[string]any{"message_id": messageID}); e != nil {
			return e
		}
		created = true
		return enqueueCase(ctx, tx, id, "operator-reply:"+r.RequestID, at)
	})
	return
}
func (s *Store) CaseAttempts(ctx context.Context, id domain.InvestigationID) ([]domain.Investigation, error) {
	var root domain.InvestigationID
	if err := s.pool.QueryRow(ctx, `SELECT case_id FROM investigations WHERE id=$1`, id).Scan(&root); errors.Is(err, pgx.ErrNoRows) {
		return nil, investigations.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,status,parent_id,created_at,completed_at FROM investigations WHERE case_id=$1 ORDER BY created_at,id LIMIT 100`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Investigation{}
	for rows.Next() {
		var i domain.Investigation
		var parent *string
		if err := rows.Scan(&i.ID, &i.Status, &parent, &i.CreatedAt, &i.CompletedAt); err != nil {
			return nil, err
		}
		i.CaseID = root
		if parent != nil {
			i.ParentID = domain.InvestigationID(*parent)
		}
		result = append(result, i)
	}
	return result, rows.Err()
}
