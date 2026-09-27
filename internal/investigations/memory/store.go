package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

type Store struct {
	mu             sync.Mutex
	tickets        map[domain.TicketID]domain.Ticket
	dedupe         map[string]domain.InvestigationID
	investigations map[domain.InvestigationID]domain.Investigation
	evidence       map[domain.InvestigationID][]domain.Evidence
	actions        map[domain.InvestigationID][]investigations.ActionEvaluation
	approvals      map[domain.ApprovalID]domain.Approval
	activeApproval map[domain.ActionID]domain.ApprovalID
	events         map[domain.InvestigationID][]audit.Event
}

func NewStore() *Store {
	return &Store{
		tickets: make(map[domain.TicketID]domain.Ticket), dedupe: make(map[string]domain.InvestigationID),
		investigations: make(map[domain.InvestigationID]domain.Investigation),
		evidence:       make(map[domain.InvestigationID][]domain.Evidence),
		actions:        make(map[domain.InvestigationID][]investigations.ActionEvaluation),
		approvals:      make(map[domain.ApprovalID]domain.Approval),
		activeApproval: make(map[domain.ActionID]domain.ApprovalID),
		events:         make(map[domain.InvestigationID][]audit.Event),
	}
}

func (s *Store) ReceiveTicket(_ context.Context, ticket domain.Ticket, investigation domain.Investigation) (domain.InvestigationID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(ticket.Source) + "\x00" + ticket.ExternalID
	if existing, ok := s.dedupe[key]; ok {
		return existing, false, nil
	}
	s.tickets[ticket.ID] = cloneTicket(ticket)
	s.investigations[investigation.ID] = investigation
	s.dedupe[key] = investigation.ID
	s.appendEvent(investigation.ID, audit.TicketReceived, audit.ActorConnector, string(ticket.Source), ticket.CreatedAt, map[string]any{
		"ticket_id": ticket.ID, "source": ticket.Source, "external_id": ticket.ExternalID,
	})
	return investigation.ID, true, nil
}

func (s *Store) GetInvestigation(_ context.Context, id domain.InvestigationID) (domain.Investigation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.investigations[id]
	if !ok {
		return domain.Investigation{}, investigations.ErrNotFound
	}
	return item, nil
}

func (s *Store) GetTicket(_ context.Context, id domain.InvestigationID) (domain.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.investigations[id]
	if !ok {
		return domain.Ticket{}, investigations.ErrNotFound
	}
	return cloneTicket(s.tickets[inv.TicketID]), nil
}

func (s *Store) StartInvestigation(_ context.Context, id domain.InvestigationID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.investigations[id]
	if !ok {
		return investigations.ErrNotFound
	}
	if item.Status == domain.InvestigationRunning {
		return nil
	}
	if item.Status != domain.InvestigationPending {
		return fmt.Errorf("%w: %s to RUNNING", investigations.ErrInvalidTransition, item.Status)
	}
	item.Status, item.StartedAt = domain.InvestigationRunning, &at
	s.investigations[id] = item
	s.appendEvent(id, audit.InvestigationStarted, audit.ActorSystem, "orchestrator", at, map[string]any{"prompt_version": item.PromptVersion})
	return nil
}

func (s *Store) RecordToolInvocations(_ context.Context, id domain.InvestigationID, tools []investigations.ToolInvocation, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireRunning(id); err != nil {
		return err
	}
	for _, tool := range tools {
		s.appendEvent(id, audit.ToolInvoked, audit.ActorSystem, "orchestrator", at, map[string]any{"connector": tool.Connector, "capability": tool.Capability})
	}
	return nil
}

func (s *Store) RecordEvidence(_ context.Context, id domain.InvestigationID, items []domain.Evidence, failures []investigations.CollectionFailure, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireRunning(id); err != nil {
		return err
	}
	for _, item := range items {
		copy := item
		copy.Data = append(json.RawMessage(nil), item.Data...)
		s.evidence[id] = append(s.evidence[id], copy)
		s.appendEvent(id, audit.EvidenceCollected, audit.ActorConnector, string(item.Source), at, map[string]any{
			"evidence_id": item.ID, "kind": item.Kind, "observed_at": item.ObservedAt,
		})
	}
	for _, failure := range failures {
		s.appendEvent(id, audit.EvidenceCollectionFailed, audit.ActorConnector, string(failure.Source), at, map[string]any{
			"error": failure.Error, "duration_ms": failure.Duration.Milliseconds(),
		})
	}
	return nil
}

func (s *Store) CompleteInvestigation(_ context.Context, id domain.InvestigationID, diagnosis string, provider domain.ModelProviderName, model string, evaluations []investigations.ActionEvaluation, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireRunning(id); err != nil {
		return err
	}
	item := s.investigations[id]
	item.Diagnosis, item.ModelProvider, item.Model = diagnosis, provider, model
	s.appendEvent(id, audit.DiagnosisGenerated, audit.ActorModel, string(provider), at, map[string]any{"model": model})
	for _, evaluation := range evaluations {
		s.actions[id] = append(s.actions[id], cloneEvaluation(evaluation))
		s.appendEvent(id, audit.ActionProposed, audit.ActorModel, string(provider), at, map[string]any{
			"action_id": evaluation.Action.ID, "capability": evaluation.Action.Capability,
			"arguments": evaluation.Action.Arguments, "reason": evaluation.Action.Reason,
			"digest": evaluation.Action.Digest, "evidence_ids": evaluation.Action.EvidenceIDs,
		})
		s.appendEvent(id, audit.PolicyEvaluated, audit.ActorSystem, "policy", at, map[string]any{
			"action_id": evaluation.Action.ID, "decision": evaluation.Policy.Decision, "reason": evaluation.Policy.Reason,
		})
	}
	item.Status, item.CompletedAt = domain.InvestigationCompleted, &at
	s.investigations[id] = item
	s.appendEvent(id, audit.InvestigationCompleted, audit.ActorSystem, "orchestrator", at, map[string]any{"actions_proposed": len(evaluations)})
	return nil
}

func (s *Store) FailInvestigation(_ context.Context, id domain.InvestigationID, failure string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.investigations[id]
	if !ok {
		return investigations.ErrNotFound
	}
	if item.Status == domain.InvestigationCompleted || item.Status == domain.InvestigationFailed {
		return nil
	}
	item.Status, item.Failure, item.CompletedAt = domain.InvestigationFailed, failure, &at
	s.investigations[id] = item
	s.appendEvent(id, audit.InvestigationFailed, audit.ActorSystem, "orchestrator", at, map[string]any{"error": failure})
	return nil
}

func (s *Store) Timeline(_ context.Context, id domain.InvestigationID) ([]audit.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.investigations[id]; !ok {
		return nil, investigations.ErrNotFound
	}
	items := s.events[id]
	result := make([]audit.Event, len(items))
	for i, item := range items {
		result[i] = item
		result[i].Data = append(json.RawMessage(nil), item.Data...)
	}
	return result, nil
}

func (s *Store) Actions(id domain.InvestigationID) []investigations.ActionEvaluation {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.actions[id]
	result := make([]investigations.ActionEvaluation, len(items))
	for i, item := range items {
		result[i] = cloneEvaluation(item)
	}
	return result
}

func (s *Store) CreateApproval(ctx context.Context, action domain.ProposedAction, approval domain.Approval, at time.Time) (domain.Approval, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, false, err
	}

	var persisted *investigations.ActionEvaluation
	for i := range s.actions[action.InvestigationID] {
		if s.actions[action.InvestigationID][i].Action.ID == action.ID {
			persisted = &s.actions[action.InvestigationID][i]
			break
		}
	}
	if persisted == nil || persisted.Action.InvestigationID != action.InvestigationID {
		return domain.Approval{}, false, approvals.ErrActionNotEligible
	}
	if persisted.Action.Digest != action.Digest || !persisted.Action.DigestValid() || !action.DigestValid() {
		return domain.Approval{}, false, approvals.ErrActionChanged
	}
	if persisted.Policy.Decision != domain.PolicyRequireApproval {
		return domain.Approval{}, false, approvals.ErrActionNotEligible
	}
	if approval.ID == "" || approval.ActionID != action.ID || approval.ActionDigest != action.Digest ||
		approval.Status != domain.ApprovalPending || !approval.ExpiresAt.After(at) {
		return domain.Approval{}, false, approvals.ErrActionChanged
	}

	if existingID, ok := s.activeApproval[action.ID]; ok {
		existing := s.approvals[existingID]
		if at.Before(existing.ExpiresAt) {
			return existing, false, nil
		}
		s.expireApproval(existing, action.InvestigationID, at)
	}

	s.approvals[approval.ID] = approval
	s.activeApproval[action.ID] = approval.ID
	s.appendEvent(action.InvestigationID, audit.ApprovalRequested, audit.ActorSystem, "approvals", at, map[string]any{
		"approval_id": approval.ID, "action_id": action.ID, "action_digest": action.Digest,
		"expires_at": approval.ExpiresAt,
	})
	return approval, true, nil
}

func (s *Store) GetApproval(ctx context.Context, id domain.ApprovalID) (domain.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}
	item, ok := s.approvals[id]
	if !ok {
		return domain.Approval{}, approvals.ErrNotFound
	}
	return item, nil
}

func (s *Store) DecideApproval(ctx context.Context, id domain.ApprovalID, decision domain.ApprovalStatus, actor string, at time.Time) (domain.Approval, error) {
	if err := approvals.ValidateDecisionInput(decision, actor); err != nil {
		return domain.Approval{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}
	item, ok := s.approvals[id]
	if !ok {
		return domain.Approval{}, approvals.ErrNotFound
	}
	if item.Status == domain.ApprovalExpired {
		return item, approvals.ErrExpired
	}
	if item.Status != domain.ApprovalPending {
		return item, approvals.ErrAlreadyDecided
	}
	investigationID, ok := s.actionInvestigationID(item.ActionID)
	if !ok {
		return domain.Approval{}, approvals.ErrActionNotEligible
	}
	if !at.Before(item.ExpiresAt) {
		s.expireApproval(item, investigationID, at)
		return s.approvals[id], approvals.ErrExpired
	}

	item.Status = decision
	item.Actor = actor
	item.DecidedAt = timePointer(at)
	s.approvals[id] = item
	if decision == domain.ApprovalDenied {
		delete(s.activeApproval, item.ActionID)
	}
	eventType := audit.ApprovalGranted
	if decision == domain.ApprovalDenied {
		eventType = audit.ApprovalDenied
	}
	s.appendEvent(investigationID, eventType, audit.ActorOperator, actor, at, map[string]any{
		"approval_id": item.ID, "action_id": item.ActionID, "action_digest": item.ActionDigest,
	})
	return item, nil
}

func (s *Store) actionInvestigationID(actionID domain.ActionID) (domain.InvestigationID, bool) {
	for investigationID, items := range s.actions {
		for _, item := range items {
			if item.Action.ID == actionID {
				return investigationID, true
			}
		}
	}
	return "", false
}

func (s *Store) expireApproval(item domain.Approval, investigationID domain.InvestigationID, at time.Time) {
	item.Status = domain.ApprovalExpired
	s.approvals[item.ID] = item
	delete(s.activeApproval, item.ActionID)
	s.appendEvent(investigationID, audit.ApprovalExpired, audit.ActorSystem, "approvals", at, map[string]any{
		"approval_id": item.ID, "action_id": item.ActionID, "action_digest": item.ActionDigest,
	})
}

func timePointer(value time.Time) *time.Time { return &value }

func (s *Store) requireRunning(id domain.InvestigationID) error {
	item, ok := s.investigations[id]
	if !ok {
		return investigations.ErrNotFound
	}
	if item.Status != domain.InvestigationRunning {
		return fmt.Errorf("%w: expected RUNNING, got %s", investigations.ErrInvalidTransition, item.Status)
	}
	return nil
}

func (s *Store) appendEvent(id domain.InvestigationID, eventType audit.EventType, actorType audit.ActorType, actorID string, at time.Time, data any) {
	sequence := int64(len(s.events[id]) + 1)
	s.events[id] = append(s.events[id], audit.Event{
		ID: sequence, InvestigationID: id, Sequence: sequence, Type: eventType,
		ActorType: actorType, ActorID: actorID, OccurredAt: at, Data: audit.Data(data),
	})
}

func cloneTicket(ticket domain.Ticket) domain.Ticket { return ticket }

func cloneEvaluation(item investigations.ActionEvaluation) investigations.ActionEvaluation {
	item.Action.Arguments = append(json.RawMessage(nil), item.Action.Arguments...)
	item.Action.EvidenceIDs = append([]domain.EvidenceID(nil), item.Action.EvidenceIDs...)
	return item
}
