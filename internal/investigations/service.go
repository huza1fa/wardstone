package investigations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

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
	maxIntentInputBytes     = 64 << 10
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
	RoutingRules             []specialists.RouteRule
	IntentClassification     *specialists.IntentConfig
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
	intent      specialists.IntentConfig
	candidates  []agent.IntentCandidate
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
	confidence := 1.0
	if err := (domain.RoutingDecision{Specialist: specialists.HelpDesk, Classification: "help_desk",
		Source: domain.RoutingSourceModel, Reason: "validate model identity", Confidence: &confidence,
		ModelProvider: model.Name(), Model: model.Model()}).Validate(); err != nil {
		return nil, fmt.Errorf("model identity: %w", err)
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
	dispatcher, err := specialists.NewDispatcherWithRules(config.Specialists, config.RoutingRules)
	if err != nil {
		return nil, fmt.Errorf("create dispatcher: %w", err)
	}
	intent := specialists.DefaultIntentConfig()
	if config.IntentClassification != nil {
		intent = *config.IntentClassification
	}
	if err := intent.Validate(); err != nil {
		return nil, fmt.Errorf("intent classification: %w", err)
	}
	candidates := make([]agent.IntentCandidate, 0, len(config.Specialists.Profiles()))
	for _, name := range dispatcher.Candidates() {
		profile, _ := config.Specialists.Get(name)
		candidates = append(candidates, agent.IntentCandidate{
			Specialist: name, Classification: string(name), Description: boundedString(profile.Instructions, 2000),
		})
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
	installed := make(map[domain.CapabilityName]bool, len(collectors))
	for _, collector := range collectors {
		if collector == nil {
			return nil, errors.New("installed evidence collector is required")
		}
		name, capability := collector.Name(), collector.Capability()
		definition, err := registry.Get(capability)
		if err != nil {
			return nil, fmt.Errorf("installed collector %q references unknown capability %q", name, capability)
		}
		if definition.Connector != name {
			return nil, fmt.Errorf("installed collector %q does not own capability %q", name, capability)
		}
		if definition.Effect != domain.EffectRead {
			return nil, fmt.Errorf("installed collector %q capability %q must be read-only", name, capability)
		}
		if installed[capability] {
			return nil, fmt.Errorf("duplicate installed collector capability %q", capability)
		}
		installed[capability] = true
	}
	return &Service{
		store: store, collectors: append([]connectors.EvidenceCollector(nil), collectors...),
		model: model, registry: registry, policy: policy, config: config, clock: realClock{},
		specialists: config.Specialists, dispatcher: dispatcher, intent: intent, candidates: candidates,
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
		if decision.Source == domain.RoutingSourceFallback {
			decision, err = s.classifyIntent(ctx, ticket, decision)
			if err != nil {
				return fmt.Errorf("classify ticket intent: %w", err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.store.Dispatch(ctx, id, decision, s.clock.Now()); err != nil {
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
	// The immutable ledger retains model drafts; reasoning uses the text an
	// operator actually approved, including edits, on subsequent runs.
	if reader, ok := s.store.(interface {
		ListDeliveries(context.Context, domain.InvestigationID) ([]domain.MessageDelivery, error)
	}); ok {
		deliveries, err := reader.ListDeliveries(ctx, id)
		if err != nil {
			return fmt.Errorf("load reviewed conversation: %w", err)
		}
		for i := range messages {
			for _, d := range deliveries {
				if d.MessageID == messages[i].ID && d.ReviewedBody != "" {
					messages[i].Body = d.ReviewedBody
				}
			}
		}
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
	available := make(map[domain.CapabilityName]bool)
	for _, collector := range s.collectors {
		if s.specialists.AllowsCollector(profile.Name, collector.Capability()) {
			activeCollectors = append(activeCollectors, collector)
			available[collector.Capability()] = true
		}
	}
	handoffs, warnings, err := s.caseHistory(ctx, id, profile)
	if err != nil {
		return err
	}
	for _, capability := range profile.AllowedCollectors {
		if !available[capability] {
			warnings = append(warnings, fmt.Sprintf("Collector %s is unavailable; its observations could not be refreshed.", capability))
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
			collectorTicket := ticket
			collectorTicket.Metadata = ticket.Metadata.Clone()
			var evidence []domain.Evidence
			var collectErr error
			if contextual, ok := collector.(contextualCollector); ok {
				evidence, collectErr = contextual.CollectCase(callCtx, collectorTicket, append([]domain.CaseMessage(nil), messages...))
			} else {
				evidence, collectErr = collector.Collect(callCtx, collectorTicket)
			}
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
	for i, result := range results {
		if result.err != nil {
			failures = append(failures, CollectionFailure{Source: tools[i].Connector, Capability: tools[i].Capability, Error: result.err.Error(), Duration: result.duration})
			warnings = append(warnings, fmt.Sprintf("Collection failed for %s; observations could not be refreshed.", tools[i].Capability))
			continue
		}
		if len(result.evidence) == 0 {
			warnings = append(warnings, fmt.Sprintf("Collector %s returned no observations.", tools[i].Capability))
		}
		for _, item := range result.evidence {
			item.ID = domain.NewEvidenceID()
			item.InvestigationID = id
			item.Source = tools[i].Connector
			item.Capability = tools[i].Capability
			item.Data = append(json.RawMessage(nil), item.Data...)
			if item.ObservedAt.IsZero() {
				item.ObservedAt = s.clock.Now()
			}
			evidence = append(evidence, item)
		}
	}
	if err := s.store.RecordEvidence(ctx, id, evidence, failures, s.clock.Now()); err != nil {
		return fmt.Errorf("record evidence: %w", err)
	}
	prior, err := s.store.ListEvidence(ctx, id, append([]domain.CapabilityName(nil), profile.AllowedCollectors...), maxPromptEvidence)
	if err != nil {
		return fmt.Errorf("reload case evidence: %w", err)
	}
	handoffOptions := make([]agent.IntentCandidate, 0, len(profile.HandoffTargets))
	for _, name := range profile.HandoffTargets {
		target, _ := s.specialists.Get(name)
		handoffOptions = append(handoffOptions, agent.IntentCandidate{Specialist: name, Classification: string(name), Description: boundedString(target.Instructions, 2000)})
	}
	actionContracts := make([]agent.ActionContract, 0, len(profile.AllowedCapabilities))
	for _, name := range profile.AllowedCapabilities {
		definition, _ := s.registry.Get(name)
		if len(definition.ArgumentsSchema) > 0 {
			actionContracts = append(actionContracts, agent.ActionContract{Capability: name, ArgumentsVersion: definition.ArgumentsVersion, ArgumentsSchema: definition.ArgumentsSchema})
		}
	}
	request, freshEvidence, err := s.boundedCaseRequest(agent.Request{
		InvestigationID: id, Ticket: ticket, Conversation: messages, Warnings: warnings, Handoffs: handoffs,
		Specialist: profile.Name, Instructions: profile.Instructions,
		PermittedActions: append([]domain.CapabilityName{}, profile.AllowedCapabilities...), HandoffTargets: append([]domain.SpecialistName{}, profile.HandoffTargets...),
		ResponseConstraint: agent.ConstraintForEvidence(evidence),
		HandoffOptions:     handoffOptions,
		ActionContracts:    actionContracts,
	}, evidence, prior, profile, s.clock.Now())
	if err != nil {
		return err
	}
	// Freeze citation authority before invoking a provider that can mutate its request.
	supplied := cloneEvidence(request.Evidence)

	modelCtx, cancel := context.WithTimeout(ctx, s.config.ModelTimeout)
	result, err := s.model.Diagnose(modelCtx, request)
	cancel()
	if err != nil {
		return fmt.Errorf("generate diagnosis: %w", err)
	}
	if agent.ConstraintForEvidence(evidence) == "final_report" && (result.FollowUpQuestion != "" || result.Handoff != nil) {
		return errors.New("operator-intervention evidence requires a final escalation report, not a requester question or handoff")
	}
	if len(result.FollowUpQuestion) > maxFollowUpQuestionSize {
		return fmt.Errorf("model follow-up question exceeds %d bytes", maxFollowUpQuestionSize)
	}
	if result.FollowUpQuestion != "" && (strings.TrimSpace(result.FollowUpQuestion) == "" || !utf8.ValidString(result.FollowUpQuestion) || strings.ContainsRune(result.FollowUpQuestion, 0)) {
		return errors.New("model follow-up question must be nonblank valid UTF-8 without NUL")
	}
	if result.Handoff != nil {
		if result.Diagnosis != "" || result.FollowUpQuestion != "" || len(result.Actions) != 0 || result.WorkPackage != nil {
			return errors.New("model handoff cannot include a diagnosis, question, actions, or work package")
		}
		if result.Handoff.Specialist == "" || strings.TrimSpace(result.Handoff.Reason) == "" || len(result.Handoff.Reason) > maxActionReasonSize || !utf8.ValidString(result.Handoff.Reason) || strings.ContainsRune(result.Handoff.Reason, 0) {
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
		if result.Diagnosis != "" || len(result.Actions) != 0 || result.WorkPackage != nil {
			return errors.New("model follow-up question cannot include a diagnosis, actions, or work package")
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
	result.WorkPackage = result.WorkPackage.Clone()
	if result.WorkPackage != nil {
		if err := result.WorkPackage.Validate(supplied); err != nil {
			return fmt.Errorf("validate work package: %w", err)
		}
		if result.Diagnosis == "" {
			result.Diagnosis = result.WorkPackage.Summary
		}
	}
	if strings.TrimSpace(result.Diagnosis) == "" {
		return errors.New("model returned an empty diagnosis")
	}
	if len(result.Diagnosis) > maxDiagnosisSize || !utf8.ValidString(result.Diagnosis) || strings.ContainsRune(result.Diagnosis, 0) {
		return fmt.Errorf("model diagnosis exceeds %d bytes", maxDiagnosisSize)
	}
	if len(result.Actions) > maxProposedActions {
		return fmt.Errorf("model proposed %d actions; limit is %d", len(result.Actions), maxProposedActions)
	}
	// Snapshot all proposals before calling any component that may retain or
	// mutate its input, so validators cannot rewrite later provider proposals.
	proposals := make([]agent.ProposedAction, len(result.Actions))
	for i, proposal := range result.Actions {
		proposals[i] = proposal
		proposals[i].Arguments = append(json.RawMessage(nil), proposal.Arguments...)
		proposals[i].EvidenceIDs = append([]domain.EvidenceID(nil), proposal.EvidenceIDs...)
	}

	evidenceSet := make(map[domain.EvidenceID]domain.Evidence, len(supplied))
	for _, item := range supplied {
		evidenceSet[item.ID] = item
	}
	validatedActions := make([]domain.ProposedAction, 0, len(proposals))
	for _, proposal := range proposals {
		if strings.TrimSpace(proposal.Reason) == "" || len(proposal.Reason) > maxActionReasonSize || !utf8.ValidString(proposal.Reason) || strings.ContainsRune(proposal.Reason, 0) || len(proposal.EvidenceIDs) == 0 || len(proposal.EvidenceIDs) > maxPromptEvidence {
			return errors.New("proposed actions require a reason and at least one evidence ID")
		}
		definition, err := s.registry.Get(proposal.Capability)
		if err != nil {
			return fmt.Errorf("validate proposed capability: %w", err)
		}
		seenIDs := make(map[domain.EvidenceID]bool)
		referenced := make([]domain.Evidence, 0, len(proposal.EvidenceIDs))
		for _, evidenceID := range proposal.EvidenceIDs {
			if _, ok := evidenceSet[evidenceID]; !ok {
				return fmt.Errorf("proposed action references unknown evidence %q", evidenceID)
			}
			if seenIDs[evidenceID] {
				return errors.New("proposed action contains duplicate evidence IDs")
			}
			seenIDs[evidenceID] = true
			if definition.Effect == domain.EffectMutate && (!freshEvidence[evidenceID] || !freshObservation(evidenceSet[evidenceID], s.clock.Now())) {
				return errors.New("mutation proposal requires fresh current-run evidence; historical or stale reference rejected")
			}
			referenced = append(referenced, evidenceSet[evidenceID])
		}
		action := domain.ProposedAction{
			ID: domain.NewActionID(), InvestigationID: id, Capability: proposal.Capability,
			Arguments: proposal.Arguments, Reason: proposal.Reason,
			EvidenceIDs: append([]domain.EvidenceID(nil), proposal.EvidenceIDs...), CreatedAt: s.clock.Now(),
		}
		if err := action.Seal(); err != nil {
			return fmt.Errorf("seal proposed action: %w", err)
		}
		if err := s.registry.ValidateAction(cloneAction(action)); err != nil {
			return fmt.Errorf("validate proposed action: %w", err)
		}
		if !s.specialists.AllowsCapability(profile.Name, action.Capability) {
			return fmt.Errorf("specialist %q is not permitted to propose capability %q", profile.Name, action.Capability)
		}
		for _, collector := range s.collectors {
			if validator, ok := collector.(connectors.ProposalValidator); ok {
				if err := validator.ValidateProposal(cloneAction(action), cloneEvidence(referenced)); err != nil {
					return fmt.Errorf("proposal validation failed for connector %q", collector.Name())
				}
			}
		}
		validatedActions = append(validatedActions, action)
	}
	evaluations := make([]ActionEvaluation, 0, len(validatedActions))
	for _, action := range validatedActions {
		evaluations = append(evaluations, ActionEvaluation{Action: action, Policy: s.policy.Evaluate(ctx, cloneAction(action))})
	}
	messageID := domain.NewMessageID()
	requesterSummary := result.Diagnosis
	if result.WorkPackage != nil {
		requesterSummary = result.WorkPackage.RequesterSummary
		if requesterSummary == "" {
			requesterSummary = "The investigation is complete. Please review the proposed next steps and any outstanding questions with an operator."
		}
	}
	resultMessage := &domain.CaseMessage{
		ID: messageID, InvestigationID: id, Source: ticket.Source,
		ExternalID: "wardstone:" + string(messageID), Direction: domain.MessageOutbound,
		Author: "wardstone", Body: formatResultMessage(requesterSummary, evaluations), CreatedAt: s.clock.Now(),
	}
	if err := s.store.CompleteInvestigation(ctx, id, result.Diagnosis, result.WorkPackage, s.model.Name(), s.model.Model(), evaluations, resultMessage, s.config.DeliverRequesterMessages, s.clock.Now()); err != nil {
		return fmt.Errorf("complete investigation: %w", err)
	}
	return nil
}

// classifyIntent is intentionally an availability-preserving refinement of
// the deterministic dispatcher. A missing, failed, malformed, low-confidence,
// or out-of-policy model answer keeps the ticket with Help Desk. It never
// grants capabilities; the selected profile's allowlists remain in force.
func (s *Service) classifyIntent(ctx context.Context, ticket domain.Ticket, fallback domain.RoutingDecision) (domain.RoutingDecision, error) {
	if err := ctx.Err(); err != nil {
		return domain.RoutingDecision{}, err
	}
	if !s.intent.Enabled {
		return intentFallback(fallback, "intent_disabled", "Intent classification is disabled; Help Desk will clarify the request."), nil
	}
	classifier, ok := s.model.(agent.IntentClassifier)
	if !ok {
		return intentFallback(fallback, "classifier_unavailable", "The model has no intent classifier; Help Desk will clarify the request."), nil
	}
	// Intent routing needs the request's content, not the reporter's identity or
	// internal case identifiers. Copy mutable metadata and candidates before
	// handing them to a provider, which may retain or modify its request.
	request := agent.IntentRequest{
		Ticket: domain.Ticket{Source: ticket.Source, Summary: boundedString(ticket.Summary, 8<<10),
			Description: boundedString(ticket.Description, 16<<10), Metadata: ticket.Metadata.Clone()},
		Candidates: append([]agent.IntentCandidate(nil), s.candidates...),
	}
	input, err := json.Marshal(request)
	if err != nil || len(input) > maxIntentInputBytes {
		return intentFallback(fallback, "intent_input_too_large", "The routing input exceeds the model budget; Help Desk will clarify the request."), nil
	}
	fallback.ModelProvider, fallback.Model = s.model.Name(), s.model.Model()
	modelCtx, cancel := context.WithTimeout(ctx, s.intent.Timeout)
	result, err := classifier.ClassifyIntent(modelCtx, request)
	callErr := modelCtx.Err()
	cancel()
	if err := ctx.Err(); err != nil {
		return domain.RoutingDecision{}, err
	}
	if errors.Is(callErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return intentFallback(fallback, "classifier_timeout", "Intent classification timed out; Help Desk will clarify the request."), nil
	}
	if err != nil {
		return intentFallback(fallback, "classifier_failed", "Intent classification failed; Help Desk will clarify the request."), nil
	}
	if math.IsNaN(result.Confidence) || math.IsInf(result.Confidence, 0) || result.Confidence < 0 || result.Confidence > 1 ||
		strings.TrimSpace(result.Reason) == "" || len(result.Reason) > maxActionReasonSize || !utf8.ValidString(result.Reason) {
		return intentFallback(fallback, "invalid_intent", "The model returned an invalid intent decision; Help Desk will clarify the request."), nil
	}
	if _, ok := s.specialists.Get(result.Specialist); !ok || result.Classification != string(result.Specialist) {
		return intentFallback(fallback, "invalid_intent", "The model selected an unknown role or classification; Help Desk will clarify the request."), nil
	}
	decision := domain.RoutingDecision{
		Specialist: result.Specialist, Classification: result.Classification, Source: domain.RoutingSourceModel,
		Reason: result.Reason, Confidence: &result.Confidence, ModelProvider: s.model.Name(), Model: s.model.Model(),
	}
	if err := decision.Validate(); err != nil {
		return intentFallback(fallback, "invalid_intent", "The model returned an invalid intent decision; Help Desk will clarify the request."), nil
	}
	if result.Confidence < s.intent.MinConfidence {
		fallback.Confidence = &result.Confidence
		return intentFallback(fallback, "low_confidence", "The model could not classify the request confidently; Help Desk will clarify the request."), nil
	}
	return decision, nil
}

func intentFallback(decision domain.RoutingDecision, code, reason string) domain.RoutingDecision {
	decision.FallbackCode, decision.Reason = code, reason
	return decision
}

// PreviewRouting evaluates administrator routing rules without invoking a
// model or creating a case. Operators can check Jira mappings before intake.
func (s *Service) PreviewRouting(ctx context.Context, ticket domain.Ticket) (domain.RoutingDecision, error) {
	if err := ctx.Err(); err != nil {
		return domain.RoutingDecision{}, err
	}
	if err := ticket.Validate(); err != nil {
		return domain.RoutingDecision{}, err
	}
	decision := s.dispatcher.Dispatch(ticket)
	if decision.Source == domain.RoutingSourceFallback {
		if !s.intent.Enabled {
			return intentFallback(decision, "intent_disabled", "No rule matched. Intent classification is disabled; intake will assign Help Desk."), nil
		}
		if _, ok := s.model.(agent.IntentClassifier); !ok {
			return intentFallback(decision, "classifier_unavailable", "No rule matched and the model has no intent classifier; intake will assign Help Desk."), nil
		}
		decision.Reason = "No rule matched. Intake will attempt intent classification before falling back to Help Desk; this preview does not call the model."
	}
	return decision, nil
}

func boundedString(value string, limit int) string {
	if len(value) > limit {
		for limit > 0 && !utf8.RuneStart(value[limit]) {
			limit--
		}
		return value[:limit]
	}
	return value
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
