package memory

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

func TestEvidenceMemoryProvenanceOrderingAndIsolation(t *testing.T) {
	store := NewStore()
	ctx, id, now := receiveRoutingCase(t, store)
	if err := store.StartInvestigation(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	items := []domain.Evidence{
		{ID: "evd_a", Source: "google", Capability: "google.groups.read", Data: json.RawMessage(`{"member":false}`), ObservedAt: now},
		{ID: "evd_old", Source: "google", Capability: "google.groups.read", Data: json.RawMessage(`{}`), ObservedAt: now.Add(-time.Hour)},
		{ID: "evd_b", Source: "google", Capability: "google.groups.read", Data: json.RawMessage(`{"member":true}`), ObservedAt: now},
		{ID: "evd_legacy", Source: "google", Data: json.RawMessage(`{}`), ObservedAt: now.Add(time.Hour)},
		{ID: "evd_forbidden", Source: "google", Capability: "google.directory.read", Data: json.RawMessage(`{}`), ObservedAt: now.Add(2 * time.Hour)},
	}
	if err := store.RecordEvidence(ctx, id, items, []investigations.CollectionFailure{{Source: "google", Capability: "google.groups.read", Error: "timeout", Duration: time.Second}}, now); err != nil {
		t.Fatal(err)
	}
	items[2].Data[0] = '!'
	for range 3 {
		got, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"", "google.groups.read"}, 2)
		if err != nil || len(got) != 2 || got[0].ID != "evd_b" || got[1].ID != "evd_a" || got[0].InvestigationID != id || !json.Valid(got[0].Data) {
			t.Fatalf("filtered newest evidence = %+v err=%v", got, err)
		}
		got[0].Data[0] = '!'
	}
	for _, allowed := range [][]domain.CapabilityName{nil, {""}, {"unavailable"}} {
		got, err := store.ListEvidence(ctx, id, allowed, 128)
		if err != nil || len(got) != 0 {
			t.Fatalf("unauthorized/legacy evidence disclosed: %+v err=%v", got, err)
		}
	}
	_, otherID, _ := receiveRoutingCase(t, store)
	if got, err := store.ListEvidence(ctx, otherID, []domain.CapabilityName{"google.groups.read"}, 128); err != nil || len(got) != 0 {
		t.Fatalf("cross-case evidence disclosed: %+v err=%v", got, err)
	}
	if _, err := store.ListEvidence(ctx, domain.NewInvestigationID(), nil, 128); !errors.Is(err, investigations.ErrNotFound) {
		t.Fatalf("missing case = %v", err)
	}
	if got, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"google.groups.read"}, 0); err != nil || len(got) != 1 {
		t.Fatalf("zero bound = %+v err=%v", got, err)
	}
	for range 130 {
		if err := store.RecordEvidence(ctx, id, []domain.Evidence{{ID: domain.NewEvidenceID(), Capability: "google.groups.read", Data: json.RawMessage(`{}`), ObservedAt: now}}, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"google.groups.read"}, int(^uint(0)>>1)); err != nil || len(got) != 128 {
		t.Fatalf("ridiculous bound returned %d rows err=%v", len(got), err)
	}
	events, err := store.Timeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var successes, failures int
	for _, event := range events {
		var data map[string]json.RawMessage
		switch event.Type {
		case audit.EvidenceCollected:
			successes++
		case audit.EvidenceCollectionFailed:
			failures++
		default:
			continue
		}
		if err := json.Unmarshal(event.Data, &data); err != nil || data["capability"] == nil {
			t.Fatalf("collection audit missing capability: %s err=%v", event.Data, err)
		}
		if event.Type == audit.EvidenceCollectionFailed && string(data["capability"]) != `"google.groups.read"` {
			t.Fatalf("failure provenance = %s", data["capability"])
		}
	}
	if successes != 135 || failures != 1 {
		t.Fatalf("collection audit counts = %d/%d", successes, failures)
	}
}

func TestCompletionMemoryReportIsolationAndStateGuard(t *testing.T) {
	store := NewStore()
	ctx, id, now := receiveRoutingCase(t, store)
	if err := store.StartInvestigation(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	report := &domain.WorkPackage{Version: 1, Summary: "No group membership",
		Findings: []domain.Finding{{Kind: "verified", Statement: "Requester is not a member", EvidenceIDs: []domain.EvidenceID{"evd_membership"}}},
		Unknowns: []string{"Owner authorization"}, RequesterSummary: "Request is ready for review", ExpectedImpact: "Restore access"}
	want := report.Clone()
	message := &domain.CaseMessage{ID: domain.NewMessageID(), Source: "jira", ExternalID: "draft", Direction: domain.MessageOutbound, Body: "Ready for review", CreatedAt: now}
	if err := store.CompleteInvestigation(ctx, id, "No membership", report, "local", "test", nil, message, false, now); err != nil {
		t.Fatal(err)
	}
	if message.InvestigationID != "" {
		t.Fatal("completion mutated input draft")
	}
	report.Findings[0].Statement, report.Findings[0].EvidenceIDs[0], report.Unknowns[0] = "changed", "changed", "changed"
	item, err := store.GetInvestigation(ctx, id)
	if err != nil || !reflect.DeepEqual(item.WorkPackage, want) || item.Status != domain.InvestigationCompleted {
		t.Fatalf("input report aliases store: %+v err=%v", item, err)
	}
	item.WorkPackage.Findings[0].Statement, item.WorkPackage.Findings[0].EvidenceIDs[0], item.WorkPackage.Unknowns[0] = "changed", "changed", "changed"
	item, err = store.GetInvestigation(ctx, id)
	if err != nil || !reflect.DeepEqual(item.WorkPackage, want) {
		t.Fatalf("returned report aliases store: %+v err=%v", item.WorkPackage, err)
	}
	before, err := store.Timeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteInvestigation(ctx, id, "rewrite", nil, "local", "test", nil, nil, false, now); !errors.Is(err, investigations.ErrInvalidTransition) {
		t.Fatalf("completed report rewrite = %v", err)
	}
	after, err := store.Timeline(ctx, id)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected completion changed audit: %+v err=%v", after, err)
	}
}

func TestCompletionMemoryInvalidDraftOrReportHasNoSideEffects(t *testing.T) {
	store := NewStore()
	ctx, id, now := receiveRoutingCase(t, store)
	if err := store.StartInvestigation(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	report := &domain.WorkPackage{Version: 1, Summary: "Investigation summary", Findings: []domain.Finding{}, Unknowns: []string{}}
	action := investigations.ActionEvaluation{Action: domain.ProposedAction{ID: domain.NewActionID(), Arguments: json.RawMessage(`{}`)}}
	before, err := store.Timeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		report *domain.WorkPackage
		draft  *domain.CaseMessage
	}{{report, &domain.CaseMessage{}}, {&domain.WorkPackage{Version: 2, Summary: "Unsupported version"}, nil}} {
		if err := store.CompleteInvestigation(ctx, id, "not committed", test.report, "local", "test", []investigations.ActionEvaluation{action}, test.draft, false, now); err == nil {
			t.Fatal("invalid completion succeeded")
		}
		item, err := store.GetInvestigation(ctx, id)
		if err != nil || item.WorkPackage != nil || item.Diagnosis != "" || item.Status != domain.InvestigationRunning || len(store.Actions(id)) != 0 {
			t.Fatalf("invalid completion changed state: %+v err=%v", item, err)
		}
		after, err := store.Timeline(ctx, id)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("invalid completion changed audit: %+v err=%v", after, err)
		}
		messages, err := store.ListMessages(ctx, id)
		if err != nil || len(messages) != 0 {
			t.Fatalf("invalid completion saved draft: %+v err=%v", messages, err)
		}
	}
	if err := store.CompleteInvestigation(ctx, id, "legacy completion", nil, "local", "test", nil, nil, false, now); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetInvestigation(ctx, id)
	if err != nil || item.WorkPackage != nil {
		t.Fatalf("nil report should remain nil: %+v err=%v", item.WorkPackage, err)
	}
}
