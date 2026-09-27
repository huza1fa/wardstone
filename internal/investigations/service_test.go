package investigations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/investigations/memory"
	"github.com/wardstone-project/wardstone/internal/policy"
)

func TestInvestigationCompletesWithPartialEvidenceAndShadowDecision(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	registry := registryForTest(t)
	var modelRequest agent.Request
	model := modelFunc(func(_ context.Context, request agent.Request) (agent.Result, error) {
		modelRequest = request
		return agent.Result{
			Diagnosis: "The account exists but is missing the engineering group.",
			Actions: []agent.ProposedAction{{
				Capability: "test.write", Arguments: json.RawMessage(`{"user":"jane@example.com"}`),
				Reason: "Required by onboarding policy.", EvidenceIDs: []domain.EvidenceID{request.Evidence[0].ID},
			}},
		}, nil
	})
	collectors := []connectors.EvidenceCollector{
		collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			return []domain.Evidence{{Kind: "user", Summary: "user exists", Data: json.RawMessage(`{"active":true}`)}}, nil
		}},
		collectorFunc{name: "jira-history", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			return nil, errors.New("temporarily unavailable")
		}},
	}
	service := newService(t, store, collectors, model, registry, 2)
	id, created, err := service.Receive(context.Background(), ticket("JIRA-1"))
	if err != nil || !created {
		t.Fatalf("receive: created=%v err=%v", created, err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != domain.InvestigationCompleted {
		t.Fatalf("status = %s", item.Status)
	}
	if len(modelRequest.Evidence) != 1 || len(modelRequest.Warnings) != 1 {
		t.Fatalf("model received evidence=%d warnings=%d", len(modelRequest.Evidence), len(modelRequest.Warnings))
	}
	actions := store.Actions(id)
	if len(actions) != 1 || actions[0].Policy.Decision != domain.PolicyDeny || actions[0].Policy.Reason != "shadow_mode" {
		t.Fatalf("unexpected action evaluations: %+v", actions)
	}
	timeline, err := store.Timeline(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []audit.EventType{
		audit.TicketReceived, audit.InvestigationStarted, audit.ToolInvoked, audit.ToolInvoked,
		audit.EvidenceCollected, audit.EvidenceCollectionFailed, audit.DiagnosisGenerated,
		audit.ActionProposed, audit.PolicyEvaluated, audit.InvestigationCompleted,
	}
	if len(timeline) != len(wantTypes) {
		t.Fatalf("timeline length = %d, want %d: %+v", len(timeline), len(wantTypes), timeline)
	}
	for i, event := range timeline {
		if event.Sequence != int64(i+1) || event.Type != wantTypes[i] {
			t.Fatalf("event %d = sequence %d type %s, want %d %s", i, event.Sequence, event.Type, i+1, wantTypes[i])
		}
	}
}

func TestEvidenceCollectionUsesBoundedConcurrency(t *testing.T) {
	t.Parallel()
	started := make(chan int, 3)
	release := make(chan struct{}, 3)
	var active atomic.Int32
	var maximum atomic.Int32
	collectors := make([]connectors.EvidenceCollector, 3)
	for i := range collectors {
		index := i
		collectors[i] = collectorFunc{name: domain.ConnectorName(fmt.Sprintf("collector-%d", index)), collect: func(ctx context.Context, _ domain.Ticket) ([]domain.Evidence, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			started <- index
			select {
			case <-release:
				return []domain.Evidence{{Kind: "test", Summary: "ok", Data: json.RawMessage(`{}`)}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	}
	store := memory.NewStore()
	registry := registryForTest(t)
	service := newService(t, store, collectors, modelFunc(noActionDiagnosis), registry, 2)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-2"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.Run(context.Background(), id) }()
	<-started
	<-started
	select {
	case third := <-started:
		t.Fatalf("collector %d started before capacity was released", third)
	default:
	}
	release <- struct{}{}
	<-started
	release <- struct{}{}
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", maximum.Load())
	}
}

func TestCancellationStopsCollectorAndLeavesInvestigationRecoverable(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	collector := collectorFunc{name: "blocking", collect: func(ctx context.Context, _ domain.Ticket) ([]domain.Evidence, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	store := memory.NewStore()
	service := newService(t, store, []connectors.EvidenceCollector{collector}, modelFunc(noActionDiagnosis), registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-3"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, id) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context canceled", err)
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != domain.InvestigationRunning {
		t.Fatalf("status = %s, want RUNNING for lease-based recovery", item.Status)
	}
}

func TestCompletedInvestigationIsIdempotentOnReclaimedJob(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	var calls atomic.Int32
	model := modelFunc(func(context.Context, agent.Request) (agent.Result, error) {
		calls.Add(1)
		return agent.Result{Diagnosis: "Complete."}, nil
	})
	service := newService(t, store, nil, model, registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-RECLAIM"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	before, err := store.Timeline(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after, err := store.Timeline(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(after) != len(before) {
		t.Fatalf("reclaimed completion reran work: calls=%d events before=%d after=%d", calls.Load(), len(before), len(after))
	}
}

func TestActionWithoutEvidenceIsRejected(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	model := modelFunc(func(context.Context, agent.Request) (agent.Result, error) {
		return agent.Result{Diagnosis: "Diagnosis.", Actions: []agent.ProposedAction{{
			Capability: "test.write", Arguments: json.RawMessage(`{}`), Reason: "unsupported",
		}}}, nil
	})
	service := newService(t, store, nil, model, registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-UNGROUNDED"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err == nil {
		t.Fatal("expected ungrounded action to be rejected")
	}
}

func TestConcurrentDuplicateTicketDeliveryCreatesOneInvestigation(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	service := newService(t, store, nil, modelFunc(noActionDiagnosis), registryForTest(t), 1)
	const deliveries = 32
	ids := make(chan domain.InvestigationID, deliveries)
	created := make(chan bool, deliveries)
	errs := make(chan error, deliveries)
	var group sync.WaitGroup
	group.Add(deliveries)
	for i := 0; i < deliveries; i++ {
		go func() {
			defer group.Done()
			id, wasCreated, err := service.Receive(context.Background(), ticket("JIRA-DUP"))
			ids <- id
			created <- wasCreated
			errs <- err
		}()
	}
	group.Wait()
	close(ids)
	close(created)
	close(errs)
	var first domain.InvestigationID
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("duplicate delivery returned different IDs: %s and %s", first, id)
		}
	}
	createdCount := 0
	for value := range created {
		if value {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
}

type collectorFunc struct {
	name    domain.ConnectorName
	collect func(context.Context, domain.Ticket) ([]domain.Evidence, error)
}

func (c collectorFunc) Name() domain.ConnectorName { return c.name }
func (c collectorFunc) Capability() domain.CapabilityName {
	return domain.CapabilityName(c.name + ".collect")
}
func (c collectorFunc) Collect(ctx context.Context, ticket domain.Ticket) ([]domain.Evidence, error) {
	return c.collect(ctx, ticket)
}

type modelFunc func(context.Context, agent.Request) (agent.Result, error)

func (modelFunc) Name() domain.ModelProviderName { return "fake" }
func (modelFunc) Model() string                  { return "fake-v1" }
func (f modelFunc) Diagnose(ctx context.Context, request agent.Request) (agent.Result, error) {
	return f(ctx, request)
}

func noActionDiagnosis(context.Context, agent.Request) (agent.Result, error) {
	return agent.Result{Diagnosis: "No action is needed."}, nil
}

func newService(t *testing.T, store *memory.Store, collectors []connectors.EvidenceCollector, model agent.ModelProvider, registry *capabilities.Registry, concurrency int) *investigations.Service {
	t.Helper()
	evaluator := policy.New(domain.OperatingModeShadow, registry, map[domain.CapabilityName]policy.RuleMode{"test.read": policy.RuleAllow, "test.write": policy.RuleAllow})
	service, err := investigations.NewService(store, collectors, model, registry, evaluator, investigations.Config{
		MaxConcurrentCollectors: concurrency, CollectorTimeout: time.Minute,
		ModelTimeout: time.Minute, PromptVersion: "test-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func registryForTest(t *testing.T) *capabilities.Registry {
	t.Helper()
	registry := capabilities.NewRegistry()
	if err := registry.Register(
		capabilities.Definition{Name: "test.read", Connector: "test", Description: "read", Effect: domain.EffectRead, ArgumentsVersion: 1},
		capabilities.Definition{Name: "test.write", Connector: "test", Description: "write", Effect: domain.EffectMutate, ArgumentsVersion: 1},
	); err != nil {
		t.Fatal(err)
	}
	return registry
}

func ticket(externalID string) domain.Ticket {
	return domain.Ticket{Source: "jira", ExternalID: externalID, Summary: "Access request", ReporterEmail: "jane@example.com"}
}
