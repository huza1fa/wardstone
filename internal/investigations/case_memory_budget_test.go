package investigations

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

func budgetService(t testing.TB) (*Service, specialists.Profile, time.Time) {
	t.Helper()
	registry := capabilities.NewRegistry()
	if err := registry.Register(capabilities.Definition{Name: "test.read", Connector: "test", Description: "Test observations", Effect: domain.EffectRead, ArgumentsVersion: 1}); err != nil {
		t.Fatal(err)
	}
	profile := specialists.Profile{Name: specialists.HelpDesk, Instructions: "Clarify the case", AllowedCollectors: []domain.CapabilityName{"test.read"}}
	roles, err := specialists.NewRegistry(profile)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{registry: registry, specialists: roles}, profile, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
}

func budgetObservation(id domain.EvidenceID, now time.Time) domain.Evidence {
	return domain.Evidence{ID: id, InvestigationID: "inv_budget", Source: "test", Capability: "test.read", Kind: "account", Summary: "Account exists", Data: json.RawMessage(`{"active":true}`), ObservedAt: now}
}

func budgetRequest(profile specialists.Profile) agent.Request {
	return agent.Request{InvestigationID: "inv_budget", Ticket: domain.Ticket{Summary: "Investigate access"}, Specialist: profile.Name, Instructions: profile.Instructions}
}

func TestCaseByteAccountingIncludesEscapesUTF8AndHistoricalWarnings(t *testing.T) {
	t.Parallel()
	service, profile, now := budgetService(t)
	current := budgetObservation("current", now)
	prior := make([]domain.Evidence, 128)
	for i := range prior {
		prior[i] = budgetObservation(domain.EvidenceID(fmt.Sprintf("prior_%03d", i)), now)
		prior[i].Summary = strings.Repeat("€<>&\n\r\t\"\\", 200)
		prior[i].Data = json.RawMessage(` { "detail" : "<>&€\n" } `)
	}
	request := budgetRequest(profile)
	request.Warnings = []string{"Existing warning <>&€\n"}
	request.Handoffs = []agent.HandoffContext{{From: "help_desk", To: "help_desk", Reason: "History <>&€\n", OccurredAt: now}}
	bounded, fresh, err := service.boundedCaseRequest(request, []domain.Evidence{current}, prior, profile, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxCaseContextBytes || len(bounded.Evidence) < 2 || len(bounded.Evidence) >= 128 || bounded.Evidence[0].ID != "current" || !fresh["current"] {
		t.Fatalf("budget bytes=%d count=%d fresh=%v", len(encoded), len(bounded.Evidence), fresh)
	}
	if !utf8.Valid(encoded) || !strings.Contains(strings.Join(bounded.Warnings, " "), "budget") {
		t.Fatal("invalid UTF8 or overflow warning missing")
	}
	for _, item := range bounded.Evidence[1:] {
		if item.Summary != prior[0].Summary || string(item.Data) != string(prior[0].Data) || fresh[item.ID] {
			t.Fatal("historical content or freshness changed")
		}
	}
	// The budget reflects encoded JSON bytes, not unescaped source text.
	if len(encoded) < maxCaseContextBytes-(10<<10) {
		t.Fatalf("unexpected early truncation: %d", len(encoded))
	}
}

func TestCaseByteAccountingArrayTransitionsAndInvalidUTF8(t *testing.T) {
	t.Parallel()
	service, profile, now := budgetService(t)
	for _, messages := range []int{0, 1, 2, 64, 65} {
		for _, warnings := range []int{0, 1, 2} {
			request := budgetRequest(profile)
			for i := 0; i < messages; i++ {
				request.Conversation = append(request.Conversation, domain.CaseMessage{Body: "€<>&\n\"\\"})
			}
			for i := 0; i < warnings; i++ {
				request.Warnings = append(request.Warnings, "Warning €<>&\n")
			}
			for _, count := range []int{0, 1, 2} {
				var observations []domain.Evidence
				for i := 0; i < count; i++ {
					observations = append(observations, budgetObservation(domain.EvidenceID(fmt.Sprintf("ev_%d", i)), now))
				}
				if _, _, err := service.boundedCaseRequest(request, observations, nil, profile, now); err != nil {
					t.Fatalf("messages=%d warnings=%d evidence=%d err=%v", messages, warnings, count, err)
				}
			}
		}
	}
	invalidText := budgetObservation("invalid-text", now)
	invalidText.Summary = "bad\xff"
	invalidData := budgetObservation("invalid-data", now)
	invalidData.Data = json.RawMessage("{\"detail\":\"bad\xff\"}")
	invalidJSON := budgetObservation("invalid-json", now)
	invalidJSON.Data = json.RawMessage(`{"detail":`)
	valid := budgetObservation("valid", now)
	valid.Summary = "Valid € observation"
	bounded, _, err := service.boundedCaseRequest(budgetRequest(profile), []domain.Evidence{invalidText, invalidData, invalidJSON, valid}, nil, profile, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Evidence) != 1 || bounded.Evidence[0].ID != "valid" || !strings.Contains(strings.Join(bounded.Warnings, " "), "budget") {
		t.Fatalf("invalid observation supplied: %+v", bounded)
	}
}

func TestCaseByteAccountingDropsOldestConversationForBaseOverflow(t *testing.T) {
	t.Parallel()
	service, profile, now := budgetService(t)
	request := budgetRequest(profile)
	request.Ticket.Description = strings.Repeat("x", maxCaseContextBytes-(20<<10))
	for i := 0; i < 5; i++ {
		request.Conversation = append(request.Conversation, domain.CaseMessage{Body: strings.Repeat("€", 5000)})
	}
	request.Conversation = append(request.Conversation, domain.CaseMessage{Body: "Latest reply"})
	bounded, _, err := service.boundedCaseRequest(request, nil, nil, profile, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Conversation) != 2 || bounded.Conversation[1].Body != "Latest reply" {
		t.Fatalf("conversation count=%d", len(bounded.Conversation))
	}
	request.Ticket.Description = strings.Repeat("<", maxCaseContextBytes)
	if _, _, err := service.boundedCaseRequest(request, nil, nil, profile, now); err == nil {
		t.Fatal("oversized base accepted")
	}
}

func BenchmarkBoundedCaseRequest128NearBudget(b *testing.B) {
	service, profile, now := budgetService(b)
	request := budgetRequest(profile)
	prior := make([]domain.Evidence, 128)
	for i := range prior {
		prior[i] = budgetObservation(domain.EvidenceID(fmt.Sprintf("prior_%03d", i)), now)
		prior[i].Summary = strings.Repeat("€<>\n", 10)
		prior[i].Data = json.RawMessage(`{"detail":"` + strings.Repeat("x", 1400) + `"}`)
	}
	bounded, _, err := service.boundedCaseRequest(request, nil, prior, profile, now)
	if err != nil {
		b.Fatal(err)
	}
	encoded, _ := json.Marshal(bounded)
	if len(bounded.Evidence) != 128 || len(encoded) < maxCaseContextBytes*9/10 {
		b.Fatalf("benchmark is not near-budget: count=%d bytes=%d", len(bounded.Evidence), len(encoded))
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(encoded)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := service.boundedCaseRequest(request, nil, prior, profile, now); err != nil {
			b.Fatal(err)
		}
	}
}
