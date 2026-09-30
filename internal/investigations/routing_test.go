package investigations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/investigations/memory"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

func TestIntentClassificationDecisionBoundaries(t *testing.T) {
	t.Parallel()
	valid := agent.IntentResult{Specialist: specialists.AccessManagement, Classification: "access_management", Confidence: .9, Reason: "An account entitlement is requested."}
	for _, test := range []struct {
		name     string
		mutate   func(*agent.IntentResult)
		err      error
		fallback string
	}{
		{name: "accepted"},
		{name: "threshold", mutate: func(r *agent.IntentResult) { r.Confidence = .75 }},
		{name: "low confidence", mutate: func(r *agent.IntentResult) { r.Confidence = .749 }, fallback: "low_confidence"},
		{name: "unknown specialist", mutate: func(r *agent.IntentResult) { r.Specialist = "root_admin" }, fallback: "invalid_intent"},
		{name: "invented classification", mutate: func(r *agent.IntentResult) { r.Classification = "administrator" }, fallback: "invalid_intent"},
		{name: "NaN", mutate: func(r *agent.IntentResult) { r.Confidence = math.NaN() }, fallback: "invalid_intent"},
		{name: "infinity", mutate: func(r *agent.IntentResult) { r.Confidence = math.Inf(1) }, fallback: "invalid_intent"},
		{name: "negative", mutate: func(r *agent.IntentResult) { r.Confidence = -.1 }, fallback: "invalid_intent"},
		{name: "above one", mutate: func(r *agent.IntentResult) { r.Confidence = 1.1 }, fallback: "invalid_intent"},
		{name: "blank reason", mutate: func(r *agent.IntentResult) { r.Reason = " \n " }, fallback: "invalid_intent"},
		{name: "oversized reason", mutate: func(r *agent.IntentResult) { r.Reason = strings.Repeat("x", 2001) }, fallback: "invalid_intent"},
		{name: "NUL reason", mutate: func(r *agent.IntentResult) { r.Reason = "reason\x00" }, fallback: "invalid_intent"},
		{name: "invalid UTF8", mutate: func(r *agent.IntentResult) { r.Reason = "\xff" }, fallback: "invalid_intent"},
		{name: "error", err: errors.New("private vendor credential must not appear in audit"), fallback: "classifier_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := memory.NewStore()
			result := valid
			if test.mutate != nil {
				test.mutate(&result)
			}
			var calls atomic.Int32
			model := intentModel{diagnose: noActionDiagnosis, classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
				calls.Add(1)
				return result, test.err
			}}
			service := newService(t, store, nil, model, registryForTest(t), 1)
			work := ticket("INTENT-BOUNDARY")
			work.Metadata = domain.TicketMetadata{}
			id, _, err := service.Receive(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			inv, err := store.GetInvestigation(context.Background(), id)
			if err != nil || inv.Routing == nil {
				t.Fatalf("investigation = %+v, err=%v", inv, err)
			}
			wantRole, wantSource := specialists.AccessManagement, domain.RoutingSourceModel
			if test.fallback != "" {
				wantRole, wantSource = specialists.HelpDesk, domain.RoutingSourceFallback
			}
			if inv.Specialist != wantRole || inv.Routing.Source != wantSource || inv.Routing.FallbackCode != test.fallback || calls.Load() != 1 {
				t.Fatalf("routing=%+v calls=%d", inv.Routing, calls.Load())
			}
			if strings.Contains(inv.Routing.Reason, "credential") {
				t.Fatal("provider error leaked into persisted routing")
			}
			if err := inv.Routing.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := service.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("completed case was reclassified")
			}
		})
	}
}

func TestRoutingRuleAndDisabledIntentNeverCallClassifier(t *testing.T) {
	t.Parallel()
	for _, match := range []bool{true, false} {
		t.Run(map[bool]string{true: "rule", false: "disabled fallback"}[match], func(t *testing.T) {
			t.Parallel()
			model := intentModel{diagnose: noActionDiagnosis, classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
				t.Error("classifier should not run")
				return agent.IntentResult{}, nil
			}}
			store := memory.NewStore()
			config := specialists.DefaultIntentConfig()
			config.Enabled = false
			service := newService(t, store, nil, model, registryForTest(t), 1, func(c *investigations.Config) { c.IntentClassification = &config })
			work := ticket("INTENT-SKIP")
			if !match {
				work.Metadata = domain.TicketMetadata{}
			}
			id, _, err := service.Receive(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			inv, _ := store.GetInvestigation(context.Background(), id)
			if match && (inv.Routing.Source != domain.RoutingSourceRule || inv.Routing.RuleName != "test-access") {
				t.Fatalf("route=%+v", inv.Routing)
			}
			if !match && (inv.Routing.Source != domain.RoutingSourceFallback || inv.Routing.FallbackCode != "intent_disabled") {
				t.Fatalf("route=%+v", inv.Routing)
			}
		})
	}
}

func TestIntentCancellationDoesNotPersistAssignment(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := intentModel{diagnose: func(context.Context, agent.Request) (agent.Result, error) {
		t.Error("diagnosis should not run")
		return agent.Result{}, nil
	},
		classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
			cancel()
			return agent.IntentResult{Specialist: specialists.HelpDesk, Classification: "help_desk", Confidence: .9, Reason: "help"}, nil
		}}
	service := newService(t, store, nil, model, registryForTest(t), 1)
	work := ticket("INTENT-CANCEL")
	work.Metadata = domain.TicketMetadata{}
	id, _, err := service.Receive(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	inv, _ := store.GetInvestigation(context.Background(), id)
	if inv.Specialist != "" || inv.Routing != nil || inv.Status != domain.InvestigationPending {
		t.Fatalf("cancelled state=%+v", inv)
	}
	timeline, _ := store.Timeline(context.Background(), id)
	if len(timeline) != 1 {
		t.Fatalf("unexpected persisted events: %+v", timeline)
	}
}

func TestIntentTimeoutFallsBackWithoutExposingError(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	model := intentModel{diagnose: noActionDiagnosis, classify: func(ctx context.Context, _ agent.IntentRequest) (agent.IntentResult, error) {
		<-ctx.Done()
		return agent.IntentResult{}, ctx.Err()
	}}
	config := specialists.DefaultIntentConfig()
	config.Timeout = time.Millisecond
	service := newService(t, store, nil, model, registryForTest(t), 1, func(c *investigations.Config) { c.IntentClassification = &config })
	work := ticket("INTENT-TIMEOUT")
	work.Metadata = domain.TicketMetadata{}
	id, _, err := service.Receive(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	inv, _ := store.GetInvestigation(context.Background(), id)
	if inv.Routing.FallbackCode != "classifier_timeout" || inv.Specialist != specialists.HelpDesk {
		t.Fatalf("routing=%+v", inv.Routing)
	}
}

func TestIntentInputOmitsIdentityAndPreservesUTF8AndIsolation(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	model := intentModel{diagnose: noActionDiagnosis, classify: func(_ context.Context, r agent.IntentRequest) (agent.IntentResult, error) {
		if r.Ticket.ReporterEmail != "" || r.Ticket.ExternalID != "" || r.Ticket.ID != "" {
			t.Errorf("identity sent to classifier: %+v", r.Ticket)
		}
		if len(r.Ticket.Description) > 16<<10 || !utf8.ValidString(r.Ticket.Description) {
			t.Error("text was not truncated safely")
		}
		r.Ticket.Metadata.Fields["category"][0] = "mutated"
		r.Candidates[0].Description = "mutated"
		return agent.IntentResult{Specialist: specialists.HelpDesk, Classification: "help_desk", Confidence: .9, Reason: "Help Desk can clarify."}, nil
	}}
	service := newService(t, store, nil, model, registryForTest(t), 1)
	work := ticket("INTENT-PRIVACY")
	work.Metadata = domain.TicketMetadata{Fields: map[string][]string{"category": {"general"}}}
	work.Description = strings.Repeat("€", 6000)
	id, _, err := service.Receive(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.GetTicket(context.Background(), id)
	if saved.Metadata.Fields["category"][0] != "general" || saved.Description != work.Description {
		t.Fatal("classifier mutated persisted ticket")
	}
}

func TestRoutingIsDurableAcrossClarificationAndHandoff(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	var classifyCalls, diagnoseCalls atomic.Int32
	model := intentModel{classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
		classifyCalls.Add(1)
		return agent.IntentResult{Specialist: specialists.HelpDesk, Classification: "help_desk", Confidence: .9, Reason: "Help Desk must clarify the system."}, nil
	}, diagnose: func(_ context.Context, r agent.Request) (agent.Result, error) {
		switch diagnoseCalls.Add(1) {
		case 1:
			return agent.Result{FollowUpQuestion: "Which application do you need?"}, nil
		case 2:
			if len(r.Conversation) != 2 {
				t.Errorf("conversation=%+v", r.Conversation)
			}
			return agent.Result{Handoff: &agent.Handoff{Specialist: specialists.AccessManagement, Reason: "The reply identifies an entitlement request."}}, nil
		default:
			return agent.Result{Diagnosis: "The access request is ready for operator review."}, nil
		}
	}}
	service := newService(t, store, nil, model, registryForTest(t), 1)
	work := ticket("INTENT-CLARIFY")
	work.Metadata = domain.TicketMetadata{}
	id, _, err := service.Receive(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	before, _ := store.GetInvestigation(context.Background(), id)
	if before.Status != domain.InvestigationWaiting {
		t.Fatalf("state=%+v", before)
	}
	_, _, err = service.ReceiveRequesterReply(context.Background(), "jira", work.ExternalID, domain.CaseMessage{
		ID: domain.NewMessageID(), Source: "jira", ExternalID: "comment-1", Author: work.ReporterEmail, Direction: domain.MessageInbound, Body: "Finance application", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetInvestigation(context.Background(), id)
	if after.Status != domain.InvestigationCompleted || after.Specialist != specialists.AccessManagement || !after.Routing.Equal(*before.Routing) || classifyCalls.Load() != 1 {
		t.Fatalf("state=%+v classifiers=%d", after, classifyCalls.Load())
	}
	timeline, _ := store.Timeline(context.Background(), id)
	routes := 0
	for _, e := range timeline {
		if e.Type == audit.DispatcherRouted {
			routes++
		}
	}
	if routes != 1 {
		t.Fatalf("route events=%d", routes)
	}
}

func TestRoutingPreviewNeverInvokesModelOrCreatesCase(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	model := intentModel{diagnose: noActionDiagnosis, classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
		t.Error("preview called classifier")
		return agent.IntentResult{}, nil
	}}
	service := newService(t, store, nil, model, registryForTest(t), 1)
	work := ticket("PREVIEW")
	work.ID = domain.NewTicketID()
	decision, err := service.PreviewRouting(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Source != domain.RoutingSourceRule {
		t.Fatalf("routing=%+v", decision)
	}
	work.Metadata = domain.TicketMetadata{}
	decision, err = service.PreviewRouting(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Source != domain.RoutingSourceFallback || !strings.Contains(decision.Reason, "does not call the model") {
		t.Fatalf("routing=%+v", decision)
	}
}

func TestConfiguredIntentThresholdAndInputBudget(t *testing.T) {
	t.Parallel()
	for _, large := range []bool{false, true} {
		t.Run(map[bool]string{false: "threshold", true: "input budget"}[large], func(t *testing.T) {
			t.Parallel()
			store := memory.NewStore()
			var calls atomic.Int32
			model := intentModel{diagnose: noActionDiagnosis, classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
				calls.Add(1)
				return agent.IntentResult{Specialist: specialists.AccessManagement, Classification: "access_management", Confidence: .9, Reason: "Access work"}, nil
			}}
			intent := specialists.DefaultIntentConfig()
			intent.MinConfidence = .95
			service := newService(t, store, nil, model, registryForTest(t), 1, func(c *investigations.Config) {
				c.IntentClassification = &intent
				if large {
					profiles := c.Specialists.Profiles()
					for i := 0; i < 40; i++ {
						profiles = append(profiles, specialists.Profile{Name: domain.SpecialistName(fmt.Sprintf("role_%03d", i)), Instructions: strings.Repeat("r", 2000)})
					}
					var err error
					c.Specialists, err = specialists.NewRegistry(profiles...)
					if err != nil {
						t.Fatal(err)
					}
				}
			})
			work := ticket("INTENT-CONFIG")
			work.Metadata = domain.TicketMetadata{}
			id, _, err := service.Receive(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			inv, _ := store.GetInvestigation(context.Background(), id)
			code := "low_confidence"
			wantCalls := int32(1)
			if large {
				code = "intent_input_too_large"
				wantCalls = 0
			}
			if inv.Routing.FallbackCode != code || inv.Specialist != specialists.HelpDesk || calls.Load() != wantCalls {
				t.Fatalf("routing=%+v calls=%d", inv.Routing, calls.Load())
			}
		})
	}
}

func TestModelAssignmentRetainsSpecialistPermissions(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	var allowedCalls, forbiddenCalls atomic.Int32
	collectors := []connectors.EvidenceCollector{
		collectorFunc{name: "allowed", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			allowedCalls.Add(1)
			return []domain.Evidence{{Kind: "test", Summary: "Allowed evidence", Data: json.RawMessage(`{}`)}}, nil
		}},
		collectorFunc{name: "forbidden", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			forbiddenCalls.Add(1)
			return nil, errors.New("forbidden collector called")
		}},
	}
	model := intentModel{classify: func(context.Context, agent.IntentRequest) (agent.IntentResult, error) {
		return agent.IntentResult{Specialist: specialists.VendorReview, Classification: "vendor_review", Confidence: 1, Reason: "Vendor review"}, nil
	},
		diagnose: func(_ context.Context, r agent.Request) (agent.Result, error) {
			if r.Specialist != specialists.VendorReview || len(r.Evidence) != 1 {
				t.Fatalf("request=%+v", r)
			}
			return agent.Result{Diagnosis: "Unsafe proposal", Actions: []agent.ProposedAction{{Capability: "test.write", Arguments: json.RawMessage(`{}`), Reason: "Attempt a capability outside the profile", EvidenceIDs: []domain.EvidenceID{r.Evidence[0].ID}}}}, nil
		}}
	service := newService(t, store, collectors, model, registryForTest(t), 1, func(c *investigations.Config) {
		profiles := c.Specialists.Profiles()
		profiles = append(profiles, specialists.Profile{Name: specialists.VendorReview, Instructions: "Review vendors", AllowedCollectors: []domain.CapabilityName{"allowed.collect"}})
		var err error
		c.Specialists, err = specialists.NewRegistry(profiles...)
		if err != nil {
			t.Fatal(err)
		}
	})
	work := ticket("INTENT-PERMISSIONS")
	work.Metadata = domain.TicketMetadata{}
	id, _, err := service.Receive(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not permitted to propose") {
		t.Fatalf("err=%v", err)
	}
	if allowedCalls.Load() != 1 || forbiddenCalls.Load() != 0 || len(store.Actions(id)) != 0 {
		t.Fatalf("collector calls allowed=%d forbidden=%d actions=%+v", allowedCalls.Load(), forbiddenCalls.Load(), store.Actions(id))
	}
}
