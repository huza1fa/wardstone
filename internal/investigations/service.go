package investigations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

const (
	maxProposedActions      = 20
	maxFollowUpQuestionSize = 4000
	maxRequesterReplySize   = 16000
	maxDiagnosisSize        = 12000
	maxActionReasonSize     = 2000
	defaultMaxHandoffs      = 3
	defaultMaxFollowUps     = 3
)

type PolicyEvaluator interface {
	Evaluate(context.Context, domain.ProposedAction) domain.PolicyResult
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type Config struct {
	MaxConcurrentCollectors  int
	CollectorTimeout         time.Duration
	ModelTimeout             time.Duration
	PromptVersion            string
	MaxHandoffs              int
	MaxFollowUpQuestions     int
	DeliverRequesterMessages bool
	Specialists              *specialists.Registry
}

type Service struct {
	store       Store
	collectors  []connectors.EvidenceCollector
	model       agent.ModelProvider
	registry    *capabilities.Registry
	policy      PolicyEvaluator
	config      Config
	clock       Clock
	specialists *specialists.Registry
	dispatcher  *specialists.Dispatcher
}

func NewService(store Store, collectors []connectors.EvidenceCollector, model agent.ModelProvider, registry *capabilities.Registry, policy PolicyEvaluator, config Config) (*Service, error) {
	if store == nil || model == nil || registry == nil || policy == nil || config.Specialists == nil {
		return nil, errors.New("store, model, capability registry, policy evaluator, and specialist registry are required")
	}
	if config.MaxConcurrentCollectors < 1 {
		return nil, errors.New("max concurrent collectors must be positive")
	}
	if config.CollectorTimeout <= 0 || config.ModelTimeout <= 0 {
		return nil, errors.New("collector and model timeouts must be positive")
	}
	if config.PromptVersion == "" {
		return nil, errors.New("prompt version is required")
	}
	if config.MaxHandoffs == 0 {
		config.MaxHandoffs = defaultMaxHandoffs
	}
	if config.MaxFollowUpQuestions == 0 {
		config.MaxFollowUpQuestions = defaultMaxFollowUps
	}
	if config.MaxHandoffs < 1 || config.MaxFollowUpQuestions < 1 {
		return nil, errors.New("handoff and follow-up question limits must be positive")
	}
	dispatcher, err := specialists.NewDispatcher(config.Specialists)
	if err != nil {
		return nil, fmt.Errorf("create dispatcher: %w", err)
	}
	for _, profile := range config.Specialists.Profiles() {
		for _, name := range profile.AllowedCollectors {
			definition, err := registry.Get(name)
			if err != nil {
				return nil, fmt.Errorf("specialist %q references unknown capability %q: %w", profile.Name, name, err)
			}
			if definition.Effect != domain.EffectRead {
				return nil, fmt.Errorf("specialist %q configures mutating collector capability %q", profile.Name, name)
			}
		}
		for _, name := range profile.AllowedCapabilities {
			if _, err := registry.Get(name); err != nil {
				return nil, fmt.Errorf("specialist %q references unknown capability %q: %w", profile.Name, name, err)
			}
		}
	}
	return &Service{
		store: store, collectors: append([]connectors.EvidenceCollector(nil), collectors...),
		model: model, registry: registry, policy: policy, config: config, clock: realClock{},
		specialists: config.Specialists, dispatcher: dispatcher,
	}, nil
}

func (s *Service) SetClockForTest(clock Clock) {
	s.clock = clock
}

func (s *Service) Receive(ctx context.Context, ticket domain.Ticket) (domain.InvestigationID, bool, error) {
	if ticket.ID == "" {
		ticket.ID = domain.NewTicketID()
	}
	if ticket.CreatedAt.IsZero() {
		ticket.CreatedAt = s.clock.Now()
	}
	if err := ticket.Validate(); err != nil {
		return "", false, err
	}
	investigation := domain.Investigation{
		ID: domain.NewInvestigationID(), TicketID: ticket.ID,
		Status: domain.InvestigationPending, PromptVersion: s.config.PromptVersion,
		CreatedAt: s.clock.Now(),
	}
	return s.store.ReceiveTicket(ctx, ticket, investigation)
}

// ReceiveRequesterReply records one connector-delivered reply and schedules a
// new investigation run when it answers an outstanding question. The store
// makes the connector message ID idempotent, so webhook retries are safe.
func (s *Service) ReceiveRequesterReply(ctx context.Context, ticketSource domain.ConnectorName, ticketExternalID string, message domain.CaseMessage) (domain.InvestigationID, bool, error) {
	if ticketSource == "" || ticketExternalID == "" || message.ID == "" || message.ExternalID == "" || message.Author == "" ||
		message.Direction != domain.MessageInbound || len(message.Body) == 0 || len(message.Body) > maxRequesterReplySize {
		return "", false, errors.New("valid requester reply source, ticket ID, message ID, and body are required")
	}
	if message.Source != ticketSource {
		return "", false, errors.New("requester reply source must match ticket source")
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = s.clock.Now()
	}
	return s.store.ReceiveRequesterReply(ctx, ticketSource, ticketExternalID, message, s.clock.Now())
}

func (s *Service) ListMessages(ctx context.Context, id domain.InvestigationID) ([]domain.CaseMessage, error) {
	return s.store.ListMessages(ctx, id)
}

func (s *Service) Run(ctx context.Context, id domain.InvestigationID) (runErr error) {
	existing, err := s.store.GetInvestigation(ctx, id)
	if err != nil {
		return fmt.Errorf("load investigation: %w", err)
	}
	if existing.Status == domain.InvestigationCompleted {
		return nil
	}
	if existing.Status == domain.InvestigationWaiting {
		return nil
	}
	ticket, err := s.store.GetTicket(ctx, id)
	if err != nil {
		return fmt.Errorf("load ticket: %w", err)
	}
	if existing.Specialist == "" {
		decision := s.dispatcher.Dispatch(ticket)
		if err := s.store.Dispatch(ctx, id, decision.Specialist, decision.Classification, decision.Reason, s.clock.Now()); err != nil {
			return fmt.Errorf("dispatch investigation: %w", err)
		}
		existing.Specialist = decision.Specialist
	}
	profile, ok := s.specialists.Get(existing.Specialist)
	if !ok {
		return fmt.Errorf("investigation has unknown specialist %q", existing.Specialist)
	}
	messages, err := s.store.ListMessages(ctx, id)
	if err != nil {
		return fmt.Errorf("load conversation: %w", err)
	}
	if err := s.store.StartInvestigation(ctx, id, s.clock.Now()); err != nil {
		return fmt.Errorf("start investigation: %w", err)
	}
	defer func() {
		if runErr == nil || ctx.Err() != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.store.FailInvestigation(cleanupCtx, id, runErr.Error(), s.clock.Now())
	}()

	activeCollectors := make([]connectors.EvidenceCollector, 0, len(s.collectors))
	for _, collector := range s.collectors {
		if s.specialists.AllowsCollector(profile.Name, collector.Capability()) {
			activeCollectors = append(activeCollectors, collector)
		}
	}
	tools := make([]ToolInvocation, len(activeCollectors))
	for i, collector := range activeCollectors {
		tools[i] = ToolInvocation{Connector: collector.Name(), Capability: collector.Capability()}
	}
	if err := s.store.RecordToolInvocations(ctx, id, tools, s.clock.Now()); err != nil {
		return fmt.Errorf("record tool invocations: %w", err)
	}

	type collectorResult struct {
		evidence []domain.Evidence
		err      error
		duration time.Duration
	}
	results := make([]collectorResult, len(activeCollectors))
	group, groupCtx := errgroup.WithContext(ctx)
	semaphore := make(chan struct{}, s.config.MaxConcurrentCollectors)
	var schedulingErr error
	for i, collector := range activeCollectors {
		select {
		case semaphore <- struct{}{}:
		case <-groupCtx.Done():
			schedulingErr = groupCtx.Err()
		}
		if schedulingErr != nil {
			break
		}
		i, collector := i, collector
		group.Go(func() error {
			defer func() { <-semaphore }()
			started := s.clock.Now()
			callCtx, cancel := context.WithTimeout(groupCtx, s.config.CollectorTimeout)
			defer cancel()
			evidence, collectErr := collector.Collect(callCtx, ticket)
			results[i] = collectorResult{evidence: evidence, err: collectErr, duration: s.clock.Now().Sub(started)}
			return nil // Collector errors are evidence gaps, not group cancellation.
		})
	}
	if err := group.Wait(); err != nil {
		return fmt.Errorf("collect evidence: %w", err)
	}
	if schedulingErr != nil {
		return schedulingErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var evidence []domain.Evidence
	var failures []CollectionFailure
	var warnings []string
	for i, result := range results {
		if result.err != nil {
			failures = append(failures, CollectionFailure{Source: tools[i].Connector, Error: result.err.Error(), Duration: result.duration})
			warnings = append(warnings, fmt.Sprintf("%s: %v", tools[i].Capability, result.err))
			continue
		}
		for _, item := range result.evidence {
			item.ID = domain.NewEvidenceID()
			item.InvestigationID = id
			if item.Source == "" {
				item.Source = tools[i].Connector
			}
			if item.ObservedAt.IsZero() {
				item.ObservedAt = s.clock.Now()
			}
			evidence = append(evidence, item)
		}
	}
	if err := s.store.RecordEvidence(ctx, id, evidence, failures, s.clock.Now()); err != nil {
		return fmt.Errorf("record evidence: %w", err)
	}
	if len(activeCollectors) > 0 && len(evidence) == 0 {
		return errors.New("all evidence collectors failed or returned no evidence")
	}

	modelCtx, cancel := context.WithTimeout(ctx, s.config.ModelTimeout)
	result, err := s.model.Diagnose(modelCtx, agent.Request{
		InvestigationID: id, Ticket: ticket, Evidence: evidence, Conversation: messages, Warnings: warnings,
		Specialist: profile.Name, Instructions: profile.Instructions,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("generate diagnosis: %w", err)
	}
	if len(result.FollowUpQuestion) > maxFollowUpQuestionSize {
		return fmt.Errorf("model follow-up question exceeds %d bytes", maxFollowUpQuestionSize)
	}
	if result.Handoff != nil {
		if result.Diagnosis != "" || result.FollowUpQuestion != "" || len(result.Actions) != 0 {
			return errors.New("model handoff cannot include a diagnosis, question, or actions")
		}
		if result.Handoff.Specialist == "" || strings.TrimSpace(result.Handoff.Reason) == "" || len(result.Handoff.Reason) > maxActionReasonSize {
			return errors.New("model handoff requires a target specialist and concise reason")
		}
		if !s.specialists.AllowsHandoff(profile.Name, result.Handoff.Specialist) {
			return fmt.Errorf("specialist %q cannot hand off to %q", profile.Name, result.Handoff.Specialist)
		}
		if err := s.checkLimit(ctx, id, audit.SpecialistHandedOff, s.config.MaxHandoffs, "handoff"); err != nil {
			return err
		}
		if err := s.store.Handoff(ctx, id, profile.Name, result.Handoff.Specialist, result.Handoff.Reason, s.clock.Now()); err != nil {
			return fmt.Errorf("handoff investigation: %w", err)
		}
		return nil
	}
	if result.FollowUpQuestion != "" {
		if result.Diagnosis != "" || len(result.Actions) != 0 {
			return errors.New("model follow-up question cannot include a diagnosis or actions")
		}
		if err := s.checkLimit(ctx, id, audit.RequesterQuestionAsked, s.config.MaxFollowUpQuestions, "follow-up question"); err != nil {
			return err
		}
		messageID := domain.NewMessageID()
		message := domain.CaseMessage{
			ID: messageID, InvestigationID: id, Source: ticket.Source,
			ExternalID: "wardstone:" + string(messageID), Direction: domain.MessageOutbound,
			Author: "wardstone", Body: result.FollowUpQuestion, CreatedAt: s.clock.Now(),
		}
		if err := s.store.WaitForRequester(ctx, id, message, s.config.DeliverRequesterMessages, s.clock.Now()); err != nil {
			return fmt.Errorf("wait for requester: %w", err)
		}
		return nil
	}
	if result.Diagnosis == "" {
		return errors.New("model returned an empty diagnosis")
	}
	if len(result.Diagnosis) > maxDiagnosisSize {
		return fmt.Errorf("model diagnosis exceeds %d bytes", maxDiagnosisSize)
	}
	if len(result.Actions) > maxProposedActions {
		return fmt.Errorf("model proposed %d actions; limit is %d", len(result.Actions), maxProposedActions)
	}

	evidenceSet := make(map[domain.EvidenceID]struct{}, len(evidence))
	for _, item := range evidence {
		evidenceSet[item.ID] = struct{}{}
	}
	evaluations := make([]ActionEvaluation, 0, len(result.Actions))
	for _, proposal := range result.Actions {
		if proposal.Reason == "" || len(proposal.Reason) > maxActionReasonSize || len(proposal.EvidenceIDs) == 0 {
			return errors.New("proposed actions require a reason and at least one evidence ID")
		}
		for _, evidenceID := range proposal.EvidenceIDs {
			if _, ok := evidenceSet[evidenceID]; !ok {
				return fmt.Errorf("proposed action references unknown evidence %q", evidenceID)
			}
		}
		action := domain.ProposedAction{
			ID: domain.NewActionID(), InvestigationID: id, Capability: proposal.Capability,
			Arguments: proposal.Arguments, Reason: proposal.Reason,
			EvidenceIDs: append([]domain.EvidenceID(nil), proposal.EvidenceIDs...), CreatedAt: s.clock.Now(),
		}
		if err := action.Seal(); err != nil {
			return fmt.Errorf("seal proposed action: %w", err)
		}
		if err := s.registry.ValidateAction(action); err != nil {
			return fmt.Errorf("validate proposed action: %w", err)
		}
		if !s.specialists.AllowsCapability(profile.Name, action.Capability) {
			return fmt.Errorf("specialist %q is not permitted to propose capability %q", profile.Name, action.Capability)
		}
		evaluations = append(evaluations, ActionEvaluation{Action: action, Policy: s.policy.Evaluate(ctx, action)})
	}
	messageID := domain.NewMessageID()
	resultMessage := &domain.CaseMessage{
		ID: messageID, InvestigationID: id, Source: ticket.Source,
		ExternalID: "wardstone:" + string(messageID), Direction: domain.MessageOutbound,
		Author: "wardstone", Body: formatResultMessage(result.Diagnosis, evaluations), CreatedAt: s.clock.Now(),
	}
	if err := s.store.CompleteInvestigation(ctx, id, result.Diagnosis, s.model.Name(), s.model.Model(), evaluations, resultMessage, s.config.DeliverRequesterMessages, s.clock.Now()); err != nil {
		return fmt.Errorf("complete investigation: %w", err)
	}
	return nil
}

func (s *Service) checkLimit(ctx context.Context, id domain.InvestigationID, eventType audit.EventType, limit int, name string) error {
	timeline, err := s.store.Timeline(ctx, id)
	if err != nil {
		return fmt.Errorf("load %s limit: %w", name, err)
	}
	count := 0
	for _, event := range timeline {
		if event.Type == eventType {
			count++
		}
	}
	if count >= limit {
		return fmt.Errorf("%s limit of %d reached; operator intervention is required", name, limit)
	}
	return nil
}

func formatResultMessage(diagnosis string, evaluations []ActionEvaluation) string {
	var body strings.Builder
	body.WriteString("Wardstone investigation complete.\n\nDiagnosis:\n")
	body.WriteString(diagnosis)
	if len(evaluations) == 0 {
		body.WriteString("\n\nNo action is proposed.")
		return body.String()
	}
	body.WriteString("\n\nProposed actions:\n")
	for _, evaluation := range evaluations {
		fmt.Fprintf(&body, "- %s (%s): %s\n", evaluation.Action.Capability, evaluation.Policy.Decision, evaluation.Action.Reason)
	}
	return body.String()
}
