package investigations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/investigations/memory"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

func TestAllowedEvidenceSurvivesRequesterResumeAndReportIsDurable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := memory.NewStore()
	collections, diagnoses := 0, 0
	var firstID domain.EvidenceID
	collector := collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
		collections++
		if collections > 1 {
			return nil, errors.New("private credential vendor error")
		}
		return []domain.Evidence{{Source: "forged", Capability: "forbidden.collect", Kind: "account", Summary: "Account exists", Data: json.RawMessage(`{}`)}}, nil
	}}
	model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
		diagnoses++
		if len(r.Evidence) != 1 || r.Evidence[0].Source != "directory" || r.Evidence[0].Capability != "directory.collect" {
			t.Fatalf("provenance: %+v", r.Evidence)
		}
		if diagnoses == 1 {
			firstID = r.Evidence[0].ID
			return agent.Result{FollowUpQuestion: "Which group?"}, nil
		}
		if r.Evidence[0].ID != firstID || len(r.Conversation) != 2 {
			t.Fatalf("memory lost: %+v", r)
		}
		warnings := strings.Join(r.Warnings, " ")
		if !strings.Contains(warnings, "historical") || strings.Contains(warnings, "credential") {
			t.Fatalf("warnings=%s", warnings)
		}
		return agent.Result{WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Internal review details", RequesterSummary: "Your request is ready for review.", Findings: []domain.Finding{{Kind: "verified", Statement: "Account exists", EvidenceIDs: []domain.EvidenceID{firstID}}}, Unknowns: []string{"Manager approval"}}}, nil
	})
	service := newService(t, store, []connectors.EvidenceCollector{collector}, model, registryForTest(t), 1)
	id, _, err := service.Receive(ctx, ticket("MEMORY-RESUME"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, _, err = service.ReceiveRequesterReply(ctx, "jira", "MEMORY-RESUME", domain.CaseMessage{ID: domain.NewMessageID(), Source: "jira", ExternalID: "reply", Author: "jane@example.com", Direction: domain.MessageInbound, Body: "Engineering"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	inv, err := store.GetInvestigation(ctx, id)
	if err != nil || inv.WorkPackage == nil || inv.Diagnosis != "Internal review details" {
		t.Fatalf("report=%+v err=%v", inv, err)
	}
	messages, _ := store.ListMessages(ctx, id)
	if !strings.Contains(messages[2].Body, "Your request is ready for review.") || strings.Contains(messages[2].Body, "Internal review details") {
		t.Fatalf("draft=%s", messages[2].Body)
	}
}

func TestHandoffFiltersForbiddenEvidenceGapsAndReason(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	allowedCalls, forbiddenCalls, calls := 0, 0, 0
	collectors := []connectors.EvidenceCollector{
		collectorFunc{name: "allowed", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			allowedCalls++
			if allowedCalls > 1 {
				return nil, errors.New("private allowed error")
			}
			return []domain.Evidence{{Kind: "account", Summary: "Allowed fact", Data: json.RawMessage(`{}`)}}, nil
		}},
		collectorFunc{name: "forbidden", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			forbiddenCalls++
			return []domain.Evidence{{Kind: "secret", Summary: "Forbidden fact", Data: json.RawMessage(`{"private":true}`)}}, nil
		}},
		collectorFunc{name: "forbidden-gap", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			return nil, errors.New("private forbidden error")
		}},
		collectorFunc{name: "allowed-gap", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
			return nil, errors.New("private allowed gap error")
		}},
	}
	model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
		calls++
		if calls == 1 {
			return agent.Result{Handoff: &agent.Handoff{Specialist: specialists.AccessManagement, Reason: "Forbidden fact must not cross boundary"}}, nil
		}
		if len(r.Evidence) != 1 || r.Evidence[0].Summary != "Allowed fact" || len(r.Handoffs) != 1 || r.Handoffs[0].From != specialists.HelpDesk || r.Handoffs[0].OccurredAt.IsZero() {
			t.Fatalf("request=%+v", r)
		}
		data, _ := json.Marshal(r)
		if !strings.Contains(strings.Join(r.Warnings, " "), "Prior collection gap for allowed-gap.collect") {
			t.Fatal("allowed historical gap lost")
		}
		for _, forbidden := range []string{"Forbidden fact", "forbidden.collect", "forbidden-gap.collect", "private allowed error", "private forbidden error", "private allowed gap error"} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("leaked %s in %s", forbidden, data)
			}
		}
		return agent.Result{Diagnosis: "Allowed observation retained."}, nil
	})
	service := newService(t, store, collectors, model, registryForTest(t), 1, func(c *investigations.Config) {
		profiles := c.Specialists.Profiles()
		for i := range profiles {
			if profiles[i].Name == specialists.AccessManagement {
				profiles[i].AllowedCollectors = []domain.CapabilityName{"allowed.collect", "allowed-gap.collect"}
			}
		}
		var err error
		c.Specialists, err = specialists.NewRegistry(profiles...)
		if err != nil {
			t.Fatal(err)
		}
	})
	work := ticket("MEMORY-HANDOFF")
	work.Metadata = domain.TicketMetadata{}
	id, _, err := service.Receive(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if forbiddenCalls != 1 {
		t.Fatal("forbidden collector ran after handoff")
	}
}

// Deliberately returns more than the store contract permits to exercise core
// defense-in-depth, including unknown legacy and forged provenance.
type evidenceStore struct {
	*memory.Store
	items []domain.Evidence
}

func (s *evidenceStore) ListEvidence(_ context.Context, id domain.InvestigationID, _ []domain.CapabilityName, _ int) ([]domain.Evidence, error) {
	items := append([]domain.Evidence(nil), s.items...)
	for i := range items {
		items[i].InvestigationID = id
	}
	return items, nil
}

func TestCasePromptBudgetsAndCurrentPriority(t *testing.T) {
	t.Parallel()
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("large=%v", large), func(t *testing.T) {
			store := &evidenceStore{Store: memory.NewStore()}
			for i := 0; i < 200; i++ {
				summary := "Prior observation"
				if large {
					summary = strings.Repeat("x", 32<<10)
				}
				store.items = append(store.items, domain.Evidence{ID: domain.EvidenceID(fmt.Sprintf("old_%03d", i)), Source: "directory", Capability: "directory.collect", Kind: "account", Summary: summary, Data: json.RawMessage(`{}`), ObservedAt: time.Now()})
			}
			store.items = append(store.items, domain.Evidence{ID: "legacy", Source: "directory", Kind: "legacy", Summary: "hidden legacy"}, domain.Evidence{ID: "forged", Source: "other", Capability: "directory.collect", Summary: "hidden forged"}, domain.Evidence{ID: "forbidden", Source: "directory", Capability: "forbidden.collect", Summary: "hidden forbidden"})
			collector := collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
				return []domain.Evidence{{Kind: "current", Summary: "Current priority", Data: json.RawMessage(`{}`)}}, nil
			}}
			model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
				data, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				if len(data) > agent.MaxDiagnosisPromptBytes || len(r.Evidence) > 128 || len(r.Evidence) < 2 || r.Evidence[0].Summary != "Current priority" {
					t.Fatalf("budget count=%d bytes=%d", len(r.Evidence), len(data))
				}
				if !large && len(r.Evidence) != 128 {
					t.Fatalf("count=%d", len(r.Evidence))
				}
				if strings.Contains(string(data), "hidden") || !strings.Contains(strings.Join(r.Warnings, " "), "budget") {
					t.Fatal("filter or budget warning missing")
				}
				return agent.Result{Diagnosis: "Bounded memory."}, nil
			})
			service := newService(t, store, []connectors.EvidenceCollector{collector}, model, registryForTest(t), 1)
			id, _, err := service.Receive(context.Background(), ticket("MEMORY-BUDGET"))
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMutationReferencesMustBeFreshCurrentAndUnmodified(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"fresh", "old timestamp", "historical", "future", "provider mutation", "duplicate", "unknown", "ages during model"} {
		t.Run(mode, func(t *testing.T) {
			clock := &caseClock{now: time.Now()}
			store := &evidenceStore{Store: memory.NewStore(), items: []domain.Evidence{{ID: "historical", Source: "directory", Capability: "directory.collect", Kind: "account", Summary: "Past", Data: json.RawMessage(`{}`), ObservedAt: time.Now()}}}
			collector := collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
				at := time.Now()
				if mode == "ages during model" {
					at = clock.Now()
				}
				if mode == "old timestamp" {
					at = at.Add(-time.Hour)
				}
				if mode == "future" {
					at = at.Add(time.Hour)
				}
				return []domain.Evidence{{Kind: "account", Summary: "Current", Data: json.RawMessage(`{}`), ObservedAt: at}}, nil
			}}
			model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
				if mode == "ages during model" {
					clock.now = clock.now.Add(time.Hour)
				}
				ids := []domain.EvidenceID{r.Evidence[0].ID}
				switch mode {
				case "historical":
					ids[0] = "historical"
				case "provider mutation":
					r.Evidence[0].ID = "invented"
					ids[0] = "invented"
				case "duplicate":
					ids = append(ids, ids[0])
				case "unknown":
					ids[0] = "not-supplied"
				}
				return agent.Result{Diagnosis: "Proposal", Actions: []agent.ProposedAction{{Capability: "test.write", Arguments: json.RawMessage(`{}`), Reason: "Review", EvidenceIDs: ids}}}, nil
			})
			service := newService(t, store, []connectors.EvidenceCollector{collector}, model, registryForTest(t), 1)
			if mode == "ages during model" {
				service.SetClockForTest(clock)
			}
			id, _, err := service.Receive(context.Background(), ticket("MEMORY-ACTION"))
			if err != nil {
				t.Fatal(err)
			}
			err = service.Run(context.Background(), id)
			if (err == nil) != (mode == "fresh") {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
			if mode != "fresh" && len(store.Actions(id)) != 0 {
				t.Fatal("invalid proposal persisted")
			}
		})
	}
}

type caseCollector struct {
	collectorFunc
	contextual func([]domain.CaseMessage) ([]domain.Evidence, error)
}

func (c caseCollector) CollectCase(_ context.Context, _ domain.Ticket, messages []domain.CaseMessage) ([]domain.Evidence, error) {
	return c.contextual(messages)
}

func TestContextualCollectorUsesRequesterReplyInSameRun(t *testing.T) {
	t.Parallel()
	store := memory.NewStore()
	calls := 0
	collector := caseCollector{collectorFunc: collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
		t.Fatal("legacy collector called")
		return nil, nil
	}}, contextual: func(messages []domain.CaseMessage) ([]domain.Evidence, error) {
		if len(messages) == 0 {
			return nil, nil
		}
		if len(messages) != 2 || messages[1].Body != "Engineering" {
			t.Fatalf("messages=%+v", messages)
		}
		return []domain.Evidence{{Kind: "group", Summary: "Engineering resolved", Data: json.RawMessage(`{}`)}}, nil
	}}
	model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
		calls++
		if calls == 1 {
			return agent.Result{FollowUpQuestion: "Which group?"}, nil
		}
		if len(r.Evidence) != 1 {
			t.Fatal("reply not collected")
		}
		return agent.Result{Diagnosis: "Resolved."}, nil
	})
	service := newService(t, store, []connectors.EvidenceCollector{collector}, model, registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("MEMORY-CONTEXT"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	_, _, err = service.ReceiveRequesterReply(context.Background(), "jira", "MEMORY-CONTEXT", domain.CaseMessage{ID: domain.NewMessageID(), Source: "jira", ExternalID: "reply", Author: "jane@example.com", Direction: domain.MessageInbound, Body: "Engineering"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestReportValidationAndLegacyCompatibility(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"legacy", "report", "invalid citation", "assumption label", "question report", "handoff report"} {
		t.Run(mode, func(t *testing.T) {
			store := memory.NewStore()
			model := modelFunc(func(context.Context, agent.Request) (agent.Result, error) {
				r := agent.Result{WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Review is needed", Findings: []domain.Finding{{Kind: "assumption", Statement: "Access may be needed"}}}}
				switch mode {
				case "legacy":
					return agent.Result{Diagnosis: "Legacy diagnosis"}, nil
				case "invalid citation":
					r.WorkPackage.Findings[0].EvidenceIDs = []domain.EvidenceID{"hidden"}
				case "assumption label":
					r.WorkPackage.Findings[0].Kind = "fact"
				case "question report":
					r.FollowUpQuestion = "Which group?"
				case "handoff report":
					r.Handoff = &agent.Handoff{Specialist: specialists.AccessManagement, Reason: "Access"}
				}
				return r, nil
			})
			service := newService(t, store, nil, model, registryForTest(t), 1)
			id, _, err := service.Receive(context.Background(), ticket("MEMORY-REPORT"))
			if err != nil {
				t.Fatal(err)
			}
			err = service.Run(context.Background(), id)
			if (err == nil) != (mode == "legacy" || mode == "report") {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
			if mode == "report" {
				messages, _ := store.ListMessages(context.Background(), id)
				if len(messages) != 1 || strings.Contains(messages[0].Body, "Review is needed") {
					t.Fatal("internal report used as requester summary")
				}
			}
		})
	}
}

type caseClock struct{ now time.Time }

func (c *caseClock) Now() time.Time { return c.now }

func TestMissingAndFailedCollectorsPermitEvidenceFreeClarification(t *testing.T) {
	t.Parallel()
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted=%v", omitted), func(t *testing.T) {
			store := memory.NewStore()
			registry := registryForTest(t)
			var collectors []connectors.EvidenceCollector
			if !omitted {
				collectors = []connectors.EvidenceCollector{collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
					return nil, errors.New("private raw failure")
				}}}
			} else {
				if err := registry.Register(capabilities.Definition{Name: "directory.collect", Connector: "directory", Description: "omitted collector", Effect: domain.EffectRead, ArgumentsVersion: 1}); err != nil {
					t.Fatal(err)
				}
			}
			model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
				warnings := strings.Join(r.Warnings, " ")
				if len(r.Evidence) != 0 || !strings.Contains(warnings, "No allowed observations") || strings.Contains(warnings, "private") {
					t.Fatalf("request=%+v", r)
				}
				if omitted && !strings.Contains(warnings, "unavailable") {
					t.Fatal("missing collector not reported")
				}
				return agent.Result{FollowUpQuestion: "Which application?"}, nil
			})
			service := newService(t, store, collectors, model, registry, 1, func(c *investigations.Config) {
				profiles := c.Specialists.Profiles()
				for i := range profiles {
					profiles[i].AllowedCollectors = []domain.CapabilityName{"directory.collect"}
				}
				var err error
				c.Specialists, err = specialists.NewRegistry(profiles...)
				if err != nil {
					t.Fatal(err)
				}
			})
			id, _, err := service.Receive(context.Background(), ticket("MEMORY-NO-EVIDENCE"))
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			inv, _ := store.GetInvestigation(context.Background(), id)
			if inv.Status != domain.InvestigationWaiting {
				t.Fatalf("status=%s", inv.Status)
			}
		})
	}
}

func TestBudgetOmittedEvidenceCannotBeCited(t *testing.T) {
	t.Parallel()
	store := &evidenceStore{Store: memory.NewStore(), items: []domain.Evidence{{ID: "too-large", Source: "directory", Capability: "directory.collect", Summary: strings.Repeat("x", agent.MaxDiagnosisPromptBytes), Data: json.RawMessage(`{}`), ObservedAt: time.Now()}}}
	model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
		if len(r.Evidence) != 0 {
			t.Fatal("oversized observation was supplied")
		}
		return agent.Result{WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Unsupported assertion", Findings: []domain.Finding{{Kind: "verified", Statement: "Fact omitted from prompt", EvidenceIDs: []domain.EvidenceID{"too-large"}}}}}, nil
	})
	registry := registryForTest(t)
	if err := registry.Register(capabilities.Definition{Name: "directory.collect", Connector: "directory", Description: "prior collector", Effect: domain.EffectRead, ArgumentsVersion: 1}); err != nil {
		t.Fatal(err)
	}
	service := newService(t, store, nil, model, registry, 1, func(c *investigations.Config) {
		profiles := c.Specialists.Profiles()
		for i := range profiles {
			profiles[i].AllowedCollectors = []domain.CapabilityName{"directory.collect"}
		}
		var err error
		c.Specialists, err = specialists.NewRegistry(profiles...)
		if err != nil {
			t.Fatal(err)
		}
	})
	id, _, err := service.Receive(context.Background(), ticket("MEMORY-OMITTED-CITATION"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unknown evidence") {
		t.Fatalf("err=%v", err)
	}
}

type conversationStore struct {
	*memory.Store
	messages []domain.CaseMessage
}

func (s *conversationStore) ListMessages(context.Context, domain.InvestigationID) ([]domain.CaseMessage, error) {
	return s.messages, nil
}

func TestConversationBudgetRetainsNewestReplyAndCurrentEvidence(t *testing.T) {
	t.Parallel()
	store := &conversationStore{Store: memory.NewStore()}
	for i := 0; i < 100; i++ {
		store.messages = append(store.messages, domain.CaseMessage{ID: domain.MessageID(fmt.Sprintf("msg_%03d", i)), Body: strings.Repeat("€", 5000), Direction: domain.MessageInbound})
	}
	store.messages[99].Body = "Newest requester reply"
	collector := collectorFunc{name: "directory", collect: func(context.Context, domain.Ticket) ([]domain.Evidence, error) {
		return []domain.Evidence{{Kind: "account", Summary: strings.Repeat("x", 32<<10), Data: json.RawMessage(`{}`)}}, nil
	}}
	model := modelFunc(func(_ context.Context, r agent.Request) (agent.Result, error) {
		data, _ := json.Marshal(r)
		if len(data) > agent.MaxDiagnosisPromptBytes || len(r.Evidence) != 1 || len(r.Conversation) == 0 || len(r.Conversation) >= 64 || r.Conversation[len(r.Conversation)-1].Body != "Newest requester reply" {
			t.Fatalf("bytes=%d messages=%d evidence=%d", len(data), len(r.Conversation), len(r.Evidence))
		}
		return agent.Result{Diagnosis: "Budgeted conversation"}, nil
	})
	service := newService(t, store, []connectors.EvidenceCollector{collector}, model, registryForTest(t), 1)
	id, _, err := service.Receive(context.Background(), ticket("MEMORY-CONVERSATION-BUDGET"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
