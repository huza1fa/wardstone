package investigations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	"github.com/wardstone-project/wardstone/internal/specialists"
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
		audit.TicketReceived, audit.DispatcherRouted, audit.InvestigationStarted, audit.SpecialistStarted, audit.ToolInvoked, audit.ToolInvoked,
		audit.EvidenceCollected, audit.EvidenceCollectionFailed, audit.DiagnosisGenerated,
		audit.ActionProposed, audit.PolicyEvaluated, audit.SpecialistCompleted, audit.InvestigationCompleted,
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

func TestAmbiguousTicketUsesHighConfidenceIntentWithinInstalledProfiles(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	registry := registryForTest(t)
	model := intentModel{
		classify: func(_ context.Context, request agent.IntentRequest) (agent.IntentResult, error) {
			if len(request.Candidates) != 2 {
				t.Fatalf("intent candidates = %+v", request.Candidates)
			}
			return agent.IntentResult{
				Specialist: specialists.AccessManagement, Classification: "access_management", Confidence: 0.9,
				Reason: "The request is for a role assignment.",
			}, nil
		},
		diagnose: func(_ context.Context, request agent.Request) (agent.Result, error) {
			if request.Specialist != specialists.AccessManagement {
				t.Fatalf("specialist = %q, want access_management", request.Specialist)
			}
			return agent.Result{Diagnosis: "Access request is ready for review."}, nil
		},
	}
	service := newService(t, store, nil, model, registry, 1)
	id, _, err := service.Receive(context.Background(), domain.Ticket{
		Source: "jira", ExternalID: "JIRA-INTENT-1", Summary: "Please add the correct role for my new project", ReporterEmail: "jane@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil || item.Specialist != specialists.AccessManagement || item.Status != domain.InvestigationCompleted {
		t.Fatalf("investigation = %+v, err = %v", item, err)
	}
}

func TestLowConfidenceIntentSafelyFallsBackToHelpDesk(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	registry := registryForTest(t)
	model := intentModel{
		classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
			return agent.IntentResult{Specialist: specialists.AccessManagement, Classification: "access_management", Confidence: 0.2, Reason: "Unsure."}, nil
		},
		diagnose: func(_ context.Context, request agent.Request) (agent.Result, error) {
			if request.Specialist != specialists.HelpDesk {
				t.Fatalf("specialist = %q, want help_desk", request.Specialist)
			}
			return agent.Result{Diagnosis: "Need more information."}, nil
		},
	}
	service := newService(t, store, nil, model, registry, 1)
	id, _, err := service.Receive(context.Background(), domain.Ticket{Source: "jira", ExternalID: "JIRA-INTENT-2", Summary: "Something is wrong", ReporterEmail: "jane@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
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

func TestRequesterReplyResumesAWaitingInvestigationWithConversationContext(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	var calls atomic.Int32
	model := modelFunc(func(_ context.Context, request agent.Request) (agent.Result, error) {
		if calls.Add(1) == 1 {
			return agent.Result{FollowUpQuestion: "Which application and error message do you see?"}, nil
		}
		if len(request.Conversation) != 2 || request.Conversation[1].Body != "VPN gives error 691" {
			t.Fatalf("resumed model conversation = %+v", request.Conversation)
		}
		return agent.Result{Diagnosis: "The VPN credentials need to be checked."}, nil
	})
	service := newService(t, store, nil, model, registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-CONVERSATION"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil || item.Status != domain.InvestigationWaiting {
		t.Fatalf("waiting investigation = %+v, err=%v", item, err)
	}
	messages, err := store.ListMessages(context.Background(), id)
	if err != nil || len(messages) != 1 || messages[0].Direction != domain.MessageOutbound {
		t.Fatalf("question messages = %+v, err=%v", messages, err)
	}
	reply := domain.CaseMessage{ID: domain.NewMessageID(), Source: "jira", ExternalID: "comment-1", Direction: domain.MessageInbound, Author: "jane@example.com", Body: "VPN gives error 691", CreatedAt: time.Now().UTC()}
	resumedID, created, err := service.ReceiveRequesterReply(context.Background(), "jira", "JIRA-CONVERSATION", reply)
	if err != nil || !created || resumedID != id {
		t.Fatalf("receive reply: id=%s created=%v err=%v", resumedID, created, err)
	}
	if _, created, err := service.ReceiveRequesterReply(context.Background(), "jira", "JIRA-CONVERSATION", reply); err != nil || created {
		t.Fatalf("duplicate reply: created=%v err=%v", created, err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	item, err = store.GetInvestigation(context.Background(), id)
	if err != nil || item.Status != domain.InvestigationCompleted || calls.Load() != 2 {
		t.Fatalf("resumed investigation = %+v calls=%d err=%v", item, calls.Load(), err)
	}
	timeline, err := store.Timeline(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []audit.EventType{audit.RequesterQuestionAsked, audit.RequesterReplyReceived, audit.InvestigationResumed, audit.InvestigationCompleted} {
		found := false
		for _, event := range timeline {
			found = found || event.Type == wanted
		}
		if !found {
			t.Fatalf("timeline missing %s: %+v", wanted, timeline)
		}
	}
}

func TestConcurrentDuplicateRequesterRepliesResumeOnce(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	service := newService(t, store, nil, modelFunc(func(context.Context, agent.Request) (agent.Result, error) {
		return agent.Result{FollowUpQuestion: "What is the asset tag?"}, nil
	}), registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-REPLY-DUP"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	const deliveries = 32
	var group sync.WaitGroup
	var created atomic.Int32
	errs := make(chan error, deliveries)
	group.Add(deliveries)
	for i := 0; i < deliveries; i++ {
		go func() {
			defer group.Done()
			_, wasCreated, err := service.ReceiveRequesterReply(context.Background(), "jira", "JIRA-REPLY-DUP", domain.CaseMessage{
				ID: domain.NewMessageID(), Source: "jira", ExternalID: "comment-duplicate", Direction: domain.MessageInbound, Author: "jane@example.com", Body: "LT-1042", CreatedAt: time.Now().UTC(),
			})
			if wasCreated {
				created.Add(1)
			}
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created count = %d, want 1", created.Load())
	}
	messages, err := store.ListMessages(context.Background(), id)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages = %+v, err=%v", messages, err)
	}
}

func TestRequesterReplyRejectsNonRequesterAndBot(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	service := newService(t, store, nil, modelFunc(func(context.Context, agent.Request) (agent.Result, error) {
		return agent.Result{FollowUpQuestion: "What is the asset tag?"}, nil
	}), registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-REPLY-AUTHOR"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	for _, author := range []string{"wardstone", "other@example.com"} {
		_, created, err := service.ReceiveRequesterReply(context.Background(), "jira", "JIRA-REPLY-AUTHOR", domain.CaseMessage{
			ID: domain.NewMessageID(), Source: "jira", ExternalID: "comment-" + author, Direction: domain.MessageInbound,
			Author: author, Body: "LT-1042", CreatedAt: time.Now().UTC(),
		})
		if !errors.Is(err, investigations.ErrUnauthorizedReply) || created {
			t.Fatalf("author %q: created=%v err=%v", author, created, err)
		}
	}
}

func TestFollowUpQuestionLimitFailsInvestigation(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	service := newService(t, store, nil, modelFunc(func(context.Context, agent.Request) (agent.Result, error) {
		return agent.Result{FollowUpQuestion: "What is the asset tag?"}, nil
	}), registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-QUESTION-LIMIT"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := service.Run(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		_, _, err := service.ReceiveRequesterReply(context.Background(), "jira", "JIRA-QUESTION-LIMIT", domain.CaseMessage{
			ID: domain.NewMessageID(), Source: "jira", ExternalID: fmt.Sprintf("comment-%d", i), Direction: domain.MessageInbound,
			Author: "jane@example.com", Body: "LT-1042", CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Run(context.Background(), id); err == nil || !strings.Contains(err.Error(), "follow-up question limit") {
		t.Fatalf("run error = %v, want follow-up question limit", err)
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil || item.Status != domain.InvestigationFailed {
		t.Fatalf("investigation = %+v, err=%v", item, err)
	}
}

func TestQuestionCannotSmuggleDiagnosisOrAction(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	service := newService(t, store, nil, modelFunc(func(context.Context, agent.Request) (agent.Result, error) {
		return agent.Result{Diagnosis: "Do this", FollowUpQuestion: "What is the asset tag?"}, nil
	}), registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("JIRA-AMBIGUOUS-MODEL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err == nil {
		t.Fatal("expected mixed question and diagnosis to be rejected")
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil || item.Status != domain.InvestigationFailed {
		t.Fatalf("investigation = %+v, err=%v", item, err)
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

func TestHelpDeskHandoffResumesWithAccessSpecialist(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	var calls atomic.Int32
	model := modelFunc(func(_ context.Context, request agent.Request) (agent.Result, error) {
		switch calls.Add(1) {
		case 1:
			if request.Specialist != specialists.HelpDesk {
				t.Fatalf("initial specialist = %q, want help desk", request.Specialist)
			}
			return agent.Result{Handoff: &agent.Handoff{Specialist: specialists.AccessManagement, Reason: "Evidence points to a missing entitlement."}}, nil
		case 2:
			if request.Specialist != specialists.AccessManagement {
				t.Fatalf("handoff specialist = %q, want access management", request.Specialist)
			}
			return agent.Result{Diagnosis: "The account is missing its required group."}, nil
		default:
			return agent.Result{}, errors.New("unexpected model call")
		}
	})
	service := newService(t, store, nil, model, registryForTest(t), 1)
	work := ticket("JIRA-HANDOFF")
	work.Summary = "VPN connection fails"
	id, _, err := service.Receive(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil || item.Status != domain.InvestigationPending || item.Specialist != specialists.AccessManagement {
		t.Fatalf("handoff state = %+v, err=%v", item, err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	item, err = store.GetInvestigation(context.Background(), id)
	if err != nil || item.Status != domain.InvestigationCompleted || calls.Load() != 2 {
		t.Fatalf("completed handoff state = %+v calls=%d err=%v", item, calls.Load(), err)
	}
	timeline, err := store.Timeline(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []audit.EventType{audit.DispatcherRouted, audit.SpecialistStarted, audit.SpecialistHandedOff, audit.SpecialistCompleted} {
		found := false
		for _, event := range timeline {
			found = found || event.Type == wanted
		}
		if !found {
			t.Fatalf("timeline missing %s: %+v", wanted, timeline)
		}
	}
}

func TestHelpDeskCannotProposeAccessCapability(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	model := modelFunc(func(_ context.Context, request agent.Request) (agent.Result, error) {
		return agent.Result{Diagnosis: "Try adding the user to the group.", Actions: []agent.ProposedAction{{
			Capability: "test.write", Arguments: json.RawMessage(`{"user":"jane@example.com"}`),
			Reason: "The account needs access.", EvidenceIDs: []domain.EvidenceID{request.Evidence[0].ID},
		}}}, nil
	})
	collector := collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
		return []domain.Evidence{{Kind: "user", Summary: "user exists", Data: json.RawMessage(`{}`)}}, nil
	}}
	service := newService(t, store, []connectors.EvidenceCollector{collector}, model, registryForTest(t), 1)
	work := ticket("JIRA-HELP-DESK-BOUNDARY")
	work.Summary = "VPN connection fails"
	id, _, err := service.Receive(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err == nil {
		t.Fatal("expected unauthorized specialist capability to be rejected")
	}
	item, err := store.GetInvestigation(context.Background(), id)
	if err != nil || item.Status != domain.InvestigationFailed {
		t.Fatalf("investigation = %+v, err=%v", item, err)
	}
}

func TestServiceRejectsMutatingCollectorProfile(t *testing.T) {
	t.Parallel()
	profiles, err := specialists.NewRegistry(specialists.Profile{
		Name: specialists.HelpDesk, Instructions: "Help users.", AllowedCollectors: []domain.CapabilityName{"test.write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = investigations.NewService(memory.NewStore(), nil, modelFunc(noActionDiagnosis), registryForTest(t),
		policy.New(domain.OperatingModeShadow, registryForTest(t), nil), investigations.Config{
			MaxConcurrentCollectors: 1, CollectorTimeout: time.Second, ModelTimeout: time.Second,
			PromptVersion: "test-v1", Specialists: profiles,
		})
	if err == nil || !strings.Contains(err.Error(), "mutating collector") {
		t.Fatalf("error = %v, want mutating collector rejection", err)
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

type intentModel struct {
	diagnose func(context.Context, agent.Request) (agent.Result, error)
	classify func(context.Context, agent.IntentRequest) (agent.IntentResult, error)
}

func (intentModel) Name() domain.ModelProviderName { return "fake" }
func (intentModel) Model() string                  { return "fake-v1" }
func (m intentModel) Diagnose(ctx context.Context, request agent.Request) (agent.Result, error) {
	return m.diagnose(ctx, request)
}
func (m intentModel) ClassifyIntent(ctx context.Context, request agent.IntentRequest) (agent.IntentResult, error) {
	return m.classify(ctx, request)
}

func noActionDiagnosis(context.Context, agent.Request) (agent.Result, error) {
	return agent.Result{Diagnosis: "No action is needed."}, nil
}

func newService(t *testing.T, store *memory.Store, collectors []connectors.EvidenceCollector, model agent.ModelProvider, registry *capabilities.Registry, concurrency int) *investigations.Service {
	t.Helper()
	evaluator := policy.New(domain.OperatingModeShadow, registry, map[domain.CapabilityName]policy.RuleMode{"test.read": policy.RuleAllow, "test.write": policy.RuleAllow})
	collectorCapabilities := make([]domain.CapabilityName, 0, len(collectors))
	for _, collector := range collectors {
		collectorCapabilities = append(collectorCapabilities, collector.Capability())
		if _, err := registry.Get(collector.Capability()); errors.Is(err, capabilities.ErrNotFound) {
			if err := registry.Register(capabilities.Definition{Name: collector.Capability(), Connector: collector.Name(), Description: "test collector", Effect: domain.EffectRead, ArgumentsVersion: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	profiles, err := specialists.NewRegistry(
		specialists.Profile{
			Name: specialists.HelpDesk, Instructions: "Test help desk profile.",
			AllowedCollectors: collectorCapabilities,
			HandoffTargets:    []domain.SpecialistName{specialists.AccessManagement},
		},
		specialists.Profile{
			Name: specialists.AccessManagement, Instructions: "Test access profile.",
			AllowedCollectors:   collectorCapabilities,
			AllowedCapabilities: []domain.CapabilityName{"test.read", "test.write"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := investigations.NewService(store, collectors, model, registry, evaluator, investigations.Config{
		MaxConcurrentCollectors: concurrency, CollectorTimeout: time.Minute,
		ModelTimeout: time.Minute, PromptVersion: "test-v1", Specialists: profiles,
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
