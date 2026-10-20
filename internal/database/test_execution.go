package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/connectors/testgroup"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/executor"
)

func loadTestAction(ctx context.Context, q rowQuerier, id domain.ActionID) (domain.ProposedAction, error) {
	var a domain.ProposedAction
	err := q.QueryRow(ctx, `SELECT p.id,p.investigation_id,p.capability,p.arguments,p.reason,p.evidence_ids,p.action_digest,p.created_at FROM proposed_actions p JOIN investigations i ON i.id=p.investigation_id JOIN tickets t ON t.id=i.ticket_id WHERE p.id=$1 AND p.capability='test.groups.add_member' AND p.policy_decision='REQUIRE_APPROVAL' AND t.source='test' AND i.status='COMPLETED'`, id).Scan(&a.ID, &a.InvestigationID, &a.Capability, &a.Arguments, &a.Reason, &a.EvidenceIDs, &a.Digest, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, executor.ErrConflict
	}
	return a, err
}
func loadTestExecution(ctx context.Context, q rowQuerier, id domain.ExecutionID) (domain.Execution, error) {
	var e domain.Execution
	err := q.QueryRow(ctx, `SELECT id,action_id,idempotency_key,status,attempt,result,error,created_at,completed_at FROM executions WHERE id=$1`, id).Scan(&e.ID, &e.ActionID, &e.IdempotencyKey, &e.Status, &e.Attempt, &e.Result, &e.Error, &e.CreatedAt, &e.CompletedAt)
	return e, err
}
func (s *Store) ProposeTestAction(ctx context.Context, p executor.Proposal, lifetime time.Duration, at time.Time) (result executor.Summary, created bool, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	if lifetime <= 0 || lifetime > 7*24*time.Hour {
		return result, false, cases.ErrInvalid
	}
	digest := cases.Digest(p)
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		previous, e := caseCommand(ctx, tx, "test-propose", p.RequestID, digest)
		if e != nil {
			return e
		}
		if previous != "" {
			if e = tx.QueryRow(ctx, `SELECT id FROM proposed_actions WHERE investigation_id=$1`, previous).Scan(&result.Action.ID); e != nil {
				return e
			}
			return nil
		}
		id := domain.NewInvestigationID()
		ticket := domain.NewTicketID()
		var version int64
		if e = tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM test_group_state WHERE group_name=$1),0)`, p.Group).Scan(&version); e != nil {
			return e
		}
		args, _ := json.Marshal(testgroup.Arguments{Group: p.Group, Member: p.Member, ExpectedVersion: version})
		action := domain.ProposedAction{ID: domain.NewActionID(), InvestigationID: id, Capability: testgroup.AddMember, Arguments: args, Reason: p.Reason, EvidenceIDs: []domain.EvidenceID{}, CreatedAt: at}
		if e = action.Seal(); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO tickets(id,source,external_id,summary,created_at) VALUES($1,'test',$2,$3,$4)`, ticket, p.RequestID, "Synthetic group addition: "+p.Group+" / "+p.Member, at); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO investigations(id,ticket_id,status,prompt_version,diagnosis,created_at,started_at,completed_at) VALUES($1,$2,'COMPLETED','test-execution-v1','Synthetic action proposed. Approval and independent verification are required; no real infrastructure is connected.',$3,$3,$3)`, id, ticket, at); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO proposed_actions(id,investigation_id,capability,arguments,reason,evidence_ids,action_digest,policy_decision,policy_reason,created_at) VALUES($1,$2,$3,$4,$5,'[]',$6,'REQUIRE_APPROVAL','isolated_test_pilot',$7)`, action.ID, id, action.Capability, action.Arguments, action.Reason, action.Digest, at); e != nil {
			return e
		}
		approval := domain.Approval{ID: domain.NewApprovalID(), ActionID: action.ID, ActionDigest: action.Digest, Status: domain.ApprovalPending, ExpiresAt: at.Add(lifetime)}
		if _, e = tx.Exec(ctx, `INSERT INTO approvals(id,action_id,action_digest,status,expires_at) VALUES($1,$2,$3,'PENDING',$4)`, approval.ID, approval.ActionID, approval.ActionDigest, approval.ExpiresAt); e != nil {
			return e
		}
		if e = recordCaseCommand(ctx, tx, "test-propose", p.Command, digest, id, at); e != nil {
			return e
		}
		if e = appendEvent(ctx, tx, id, audit.ActionProposed, audit.ActorOperator, p.Actor, at, map[string]any{"action_id": action.ID, "capability": action.Capability, "arguments": action.Arguments, "digest": action.Digest, "synthetic": true}); e != nil {
			return e
		}
		if e = appendEvent(ctx, tx, id, audit.PolicyEvaluated, audit.ActorSystem, "test-pilot", at, map[string]any{"action_id": action.ID, "decision": "REQUIRE_APPROVAL", "reason": "isolated_test_pilot"}); e != nil {
			return e
		}
		if e = appendEvent(ctx, tx, id, audit.ApprovalRequested, audit.ActorSystem, "test-pilot", at, map[string]any{"approval_id": approval.ID, "action_id": action.ID, "action_digest": action.Digest, "expires_at": approval.ExpiresAt}); e != nil {
			return e
		}
		result = executor.Summary{Action: action, Approval: approval}
		created = true
		return nil
	})
	if err == nil && !created {
		result, err = s.testExecutionSummary(ctx, result.Action.ID)
	}
	return
}
func (s *Store) QueueTestExecution(ctx context.Context, id domain.ApprovalID, c cases.Command, at time.Time) (result domain.ExecutionID, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	digest := cases.Digest(struct {
		ID      domain.ApprovalID
		Command cases.Command
	}{id, c})
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		previous, e := caseCommand(ctx, tx, "test-queue", c.RequestID, digest)
		if e != nil {
			return e
		}
		if previous != "" {
			return tx.QueryRow(ctx, `SELECT e.id FROM executions e JOIN proposed_actions p ON p.id=e.action_id WHERE p.investigation_id=$1`, previous).Scan(&result)
		}
		var approval domain.Approval
		e = tx.QueryRow(ctx, `SELECT id,action_id,action_digest,status,actor,expires_at,decided_at FROM approvals WHERE id=$1`, id).Scan(&approval.ID, &approval.ActionID, &approval.ActionDigest, &approval.Status, &approval.Actor, &approval.ExpiresAt, &approval.DecidedAt)
		if errors.Is(e, pgx.ErrNoRows) {
			return executor.ErrConflict
		}
		if e != nil {
			return e
		}
		a, e := loadTestAction(ctx, tx, approval.ActionID)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `SELECT id FROM investigations WHERE id=$1 FOR UPDATE`, a.InvestigationID); e != nil {
			return e
		}
		// Existing execution is returned, even if approval expired after first start.
		e = tx.QueryRow(ctx, `SELECT id FROM executions WHERE action_id=$1`, a.ID).Scan(&result)
		if errors.Is(e, pgx.ErrNoRows) {
			if e = approvals.Validate(approval, a, at); e != nil {
				return executor.ErrConflict
			}
			result = domain.NewExecutionID()
			if _, e = tx.Exec(ctx, `INSERT INTO executions(id,action_id,approval_id,idempotency_key,status,attempt,created_at) VALUES($1,$2,$3,$4,'PENDING',1,$5)`, result, a.ID, approval.ID, "test:"+string(a.ID)+":"+a.Digest, at); e != nil {
				return e
			}
			if e = appendEvent(ctx, tx, a.InvestigationID, audit.EventType("execution.queued"), audit.ActorOperator, c.Actor, at, map[string]any{"execution_id": result, "approval_id": approval.ID, "action_digest": a.Digest, "reason": c.Reason, "synthetic": true}); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		return recordCaseCommand(ctx, tx, "test-queue", c, digest, a.InvestigationID, at)
	})
	return
}
func (s *Store) RecheckTestExecution(ctx context.Context, id domain.ExecutionID, c cases.Command, at time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	digest := cases.Digest(struct {
		ID      domain.ExecutionID
		Command cases.Command
	}{id, c})
	return s.transaction(ctx, func(tx pgx.Tx) error {
		previous, err := caseCommand(ctx, tx, "test-recheck", c.RequestID, digest)
		if err != nil || previous != "" {
			return err
		}
		e, err := loadTestExecution(ctx, tx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return executor.ErrConflict
		}
		if err != nil {
			return err
		}
		a, err := loadTestAction(ctx, tx, e.ActionID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `SELECT id FROM investigations WHERE id=$1 FOR UPDATE`, a.InvestigationID); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE executions SET status='VERIFYING',attempt=1,error='',completed_at=NULL,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND status IN('FAILED','NEEDS_RECONCILIATION')`, id)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return executor.ErrConflict
		}
		if err = recordCaseCommand(ctx, tx, "test-recheck", c, digest, a.InvestigationID, at); err != nil {
			return err
		}
		return appendEvent(ctx, tx, a.InvestigationID, audit.EventType("execution.verification_requested"), audit.ActorOperator, c.Actor, at, map[string]any{"execution_id": id, "reason": c.Reason, "read_only": true})
	})
}
func (s *Store) testExecutionSummary(ctx context.Context, id domain.ActionID) (result executor.Summary, err error) {
	result.Action, err = loadTestAction(ctx, s.pool, id)
	if err != nil {
		return
	}
	err = s.pool.QueryRow(ctx, `SELECT id,action_id,action_digest,status,actor,expires_at,decided_at FROM approvals WHERE action_id=$1 ORDER BY expires_at DESC LIMIT 1`, id).Scan(&result.Approval.ID, &result.Approval.ActionID, &result.Approval.ActionDigest, &result.Approval.Status, &result.Approval.Actor, &result.Approval.ExpiresAt, &result.Approval.DecidedAt)
	if err != nil {
		return
	}
	var executionID domain.ExecutionID
	err = s.pool.QueryRow(ctx, `SELECT id FROM executions WHERE action_id=$1`, id).Scan(&executionID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		return
	}
	if err != nil {
		return
	}
	e, eErr := loadTestExecution(ctx, s.pool, executionID)
	err = eErr
	if err != nil {
		return
	}
	result.Execution = &e
	var v domain.Verification
	err = s.pool.QueryRow(ctx, `SELECT id,execution_id,succeeded,details,verified_at FROM verifications WHERE execution_id=$1 ORDER BY verified_at DESC,id DESC LIMIT 1`, e.ID).Scan(&v.ID, &v.ExecutionID, &v.Succeeded, &v.Details, &v.VerifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		return
	}
	if err == nil {
		result.Verification = &v
	}
	return
}
func (s *Store) ListTestExecutions(ctx context.Context) ([]executor.Summary, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.id FROM proposed_actions p JOIN investigations i ON i.id=p.investigation_id JOIN tickets t ON t.id=i.ticket_id WHERE p.capability='test.groups.add_member' AND t.source='test' ORDER BY p.created_at DESC,p.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	ids := []domain.ActionID{}
	for rows.Next() {
		var id domain.ActionID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []executor.Summary{}
	for _, id := range ids {
		i, err := s.testExecutionSummary(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, i)
	}
	return result, nil
}
func (s *Store) ClaimExecution(ctx context.Context, owner string, lease time.Duration) (job executor.Job, err error) {
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		var id domain.ExecutionID
		var investigation domain.InvestigationID
		e := tx.QueryRow(ctx, `SELECT e.id,i.id FROM executions e JOIN proposed_actions p ON p.id=e.action_id JOIN investigations i ON i.id=p.investigation_id WHERE e.status IN('PENDING','RUNNING','VERIFYING') AND (e.lease_expires_at IS NULL OR e.lease_expires_at<clock_timestamp()) AND p.capability='test.groups.add_member' ORDER BY e.created_at,e.id LIMIT 1 FOR UPDATE OF i SKIP LOCKED`).Scan(&id, &investigation)
		if errors.Is(e, pgx.ErrNoRows) {
			return executor.ErrNoExecution
		}
		if e != nil {
			return e
		}
		job.Execution, e = loadTestExecution(ctx, tx, id)
		if e != nil {
			return e
		}
		var oldLease *time.Time
		var approval domain.Approval
		if e = tx.QueryRow(ctx, `SELECT e.lease_expires_at,a.id,a.action_id,a.action_digest,a.status,a.actor,a.expires_at,a.decided_at FROM executions e JOIN approvals a ON a.id=e.approval_id WHERE e.id=$1`, id).Scan(&oldLease, &approval.ID, &approval.ActionID, &approval.ActionDigest, &approval.Status, &approval.Actor, &approval.ExpiresAt, &approval.DecidedAt); e != nil {
			return e
		}
		// Reload after acquiring the case lock to fence competing claims/rechecks.
		if oldLease != nil && oldLease.After(time.Now()) {
			return executor.ErrNoExecution
		}
		if job.Execution.Status != "PENDING" && job.Execution.Status != "RUNNING" && job.Execution.Status != "VERIFYING" {
			return executor.ErrNoExecution
		}
		job.Action, e = loadTestAction(ctx, tx, job.Execution.ActionID)
		if e != nil {
			return e
		}
		fail := ""
		if !job.Action.DigestValid() || approval.ActionID != job.Action.ID || approval.ActionDigest != job.Action.Digest {
			fail = "approval_binding_invalid"
		}
		if job.Execution.Status == "PENDING" && approvals.Validate(approval, job.Action, time.Now().UTC()) != nil {
			fail = "approval_expired_or_not_granted"
		}
		if oldLease != nil && job.Execution.Attempt >= 3 {
			fail = "execution_recovery_limit"
		}
		if fail != "" {
			if _, e = tx.Exec(ctx, `UPDATE executions SET status='NEEDS_RECONCILIATION',error=$2,completed_at=now(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, id, fail); e != nil {
				return e
			}
			return appendEvent(ctx, tx, investigation, audit.ActionFailed, audit.ActorSystem, "test-executor", time.Now().UTC(), map[string]any{"execution_id": id, "error": fail, "synthetic": true})
		}
		if oldLease != nil {
			job.Execution.Attempt++
		}
		if job.Execution.Status == "PENDING" {
			job.Execution.Status = "RUNNING"
		}
		job.Owner = owner
		if _, e = tx.Exec(ctx, `UPDATE executions SET status=$2,attempt=$3,lease_owner=$4,lease_expires_at=clock_timestamp()+$5::interval WHERE id=$1`, id, job.Execution.Status, job.Execution.Attempt, owner, lease.String()); e != nil {
			return e
		}
		return appendEvent(ctx, tx, investigation, audit.EventType("execution.started"), audit.ActorSystem, "test-executor", time.Now().UTC(), map[string]any{"execution_id": id, "phase": job.Execution.Status, "attempt": job.Execution.Attempt, "synthetic": true})
	})
	if err == nil && job.Owner == "" {
		err = executor.ErrNoExecution
	}
	return
}
func executionLease(ctx context.Context, tx pgx.Tx, job executor.Job) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM investigations WHERE id=$1 FOR UPDATE`, job.Action.InvestigationID); err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM executions WHERE id=$1 AND lease_owner=$2 AND attempt=$3 AND status IN('RUNNING','VERIFYING') AND lease_expires_at>clock_timestamp())`, job.Execution.ID, job.Owner, job.Execution.Attempt).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return executor.ErrLeaseLost
	}
	return nil
}
func (s *Store) RecordMutation(ctx context.Context, job executor.Job, result json.RawMessage, at time.Time) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := executionLease(ctx, tx, job); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE executions SET status='VERIFYING',result=$2 WHERE id=$1 AND status='RUNNING'`, job.Execution.ID, result)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return executor.ErrLeaseLost
		}
		return appendEvent(ctx, tx, job.Action.InvestigationID, audit.ActionExecuted, audit.ActorConnector, "test", at, map[string]any{"execution_id": job.Execution.ID, "action_id": job.Action.ID, "status": "APPLIED_AWAITING_VERIFICATION", "synthetic": true})
	})
}
func (s *Store) FinishVerification(ctx context.Context, job executor.Job, v domain.Verification, at time.Time) error {
	if v.ID == "" || v.ExecutionID != job.Execution.ID || !json.Valid(v.Details) {
		return executor.ErrConflict
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := executionLease(ctx, tx, job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO verifications(id,execution_id,succeeded,details,verified_at) VALUES($1,$2,$3,$4,$5)`, v.ID, v.ExecutionID, v.Succeeded, v.Details, v.VerifiedAt); err != nil {
			return err
		}
		status, code := "SUCCEEDED", ""
		if !v.Succeeded {
			status, code = "FAILED", "postcondition_not_verified"
		}
		if _, err := tx.Exec(ctx, `UPDATE executions SET status=$2,error=$3,completed_at=$4,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, job.Execution.ID, status, code, at); err != nil {
			return err
		}
		return appendEvent(ctx, tx, job.Action.InvestigationID, audit.VerificationCompleted, audit.ActorConnector, "test", at, map[string]any{"execution_id": job.Execution.ID, "succeeded": v.Succeeded, "details": v.Details, "synthetic": true})
	})
}
func (s *Store) FailExecution(ctx context.Context, job executor.Job, status, code string, at time.Time) error {
	if status != "FAILED" && status != "NEEDS_RECONCILIATION" {
		return executor.ErrConflict
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := executionLease(ctx, tx, job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE executions SET status=$2,error=$3,completed_at=$4,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, job.Execution.ID, status, code, at); err != nil {
			return err
		}
		return appendEvent(ctx, tx, job.Action.InvestigationID, audit.ActionFailed, audit.ActorSystem, "test-executor", at, map[string]any{"execution_id": job.Execution.ID, "status": status, "error": code, "synthetic": true})
	})
}
