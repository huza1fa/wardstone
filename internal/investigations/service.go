package investigations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
)

const maxProposedActions = 20

type PolicyEvaluator interface {
	Evaluate(context.Context, domain.ProposedAction) domain.PolicyResult
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type Config struct {
	MaxConcurrentCollectors int
	CollectorTimeout        time.Duration
	ModelTimeout            time.Duration
	PromptVersion           string
}

type Service struct {
	store      Store
	collectors []connectors.EvidenceCollector
	model      agent.ModelProvider
	registry   *capabilities.Registry
	policy     PolicyEvaluator
	config     Config
	clock      Clock
}

func NewService(store Store, collectors []connectors.EvidenceCollector, model agent.ModelProvider, registry *capabilities.Registry, policy PolicyEvaluator, config Config) (*Service, error) {
	if store == nil || model == nil || registry == nil || policy == nil {
		return nil, errors.New("store, model, capability registry, and policy evaluator are required")
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
	return &Service{
		store: store, collectors: append([]connectors.EvidenceCollector(nil), collectors...),
		model: model, registry: registry, policy: policy, config: config, clock: realClock{},
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

func (s *Service) Run(ctx context.Context, id domain.InvestigationID) (runErr error) {
	existing, err := s.store.GetInvestigation(ctx, id)
	if err != nil {
		return fmt.Errorf("load investigation: %w", err)
	}
	if existing.Status == domain.InvestigationCompleted {
		return nil
	}
	ticket, err := s.store.GetTicket(ctx, id)
	if err != nil {
		return fmt.Errorf("load ticket: %w", err)
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

	tools := make([]ToolInvocation, len(s.collectors))
	for i, collector := range s.collectors {
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
	results := make([]collectorResult, len(s.collectors))
	group, groupCtx := errgroup.WithContext(ctx)
	semaphore := make(chan struct{}, s.config.MaxConcurrentCollectors)
	var schedulingErr error
	for i, collector := range s.collectors {
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
	if len(s.collectors) > 0 && len(evidence) == 0 {
		return errors.New("all evidence collectors failed or returned no evidence")
	}

	modelCtx, cancel := context.WithTimeout(ctx, s.config.ModelTimeout)
	result, err := s.model.Diagnose(modelCtx, agent.Request{
		InvestigationID: id, Ticket: ticket, Evidence: evidence, Warnings: warnings,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("generate diagnosis: %w", err)
	}
	if result.Diagnosis == "" {
		return errors.New("model returned an empty diagnosis")
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
		if proposal.Reason == "" || len(proposal.EvidenceIDs) == 0 {
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
		evaluations = append(evaluations, ActionEvaluation{Action: action, Policy: s.policy.Evaluate(ctx, action)})
	}
	if err := s.store.CompleteInvestigation(ctx, id, result.Diagnosis, s.model.Name(), s.model.Model(), evaluations, s.clock.Now()); err != nil {
		return fmt.Errorf("complete investigation: %w", err)
	}
	return nil
}
