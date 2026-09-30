package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/domain"
)

type fakeAPI struct {
	snapshot Snapshot
	id       string
	decision string
	actor    string
}

func (f *fakeAPI) Snapshot(context.Context) Snapshot { return f.snapshot }

func (f *fakeAPI) DecideApproval(_ context.Context, id, decision, actor string) error {
	f.id = id
	f.decision = decision
	f.actor = actor
	return nil
}

func TestModelRequiresConfirmationBeforeApprovalDecision(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeAPI{snapshot: Snapshot{Overview: admin.Overview{Status: "ok", Mode: domain.OperatingModeApproval}, Approvals: []Approval{
		{Approval: domain.Approval{ID: "approval-1", ActionID: "action-1", Status: domain.ApprovalPending, ExpiresAt: now.Add(time.Hour)}, Capability: "google.user.suspend", Reason: "first"},
		{Approval: domain.Approval{ID: "approval-2", ActionID: "action-2", Status: domain.ApprovalPending, ExpiresAt: now.Add(time.Hour)}, Capability: "google.group.add_member", Reason: "second"},
	}, LoadedAt: now}}
	model := NewModel(api, Options{Actor: "terminal-operator"})

	updated, _ := model.Update(snapshotMsg(api.snapshot))
	model = updated.(Model)
	updated, _ = model.Update(keyMsg("3"))
	model = updated.(Model)
	updated, _ = model.Update(keyMsg("j"))
	model = updated.(Model)
	updated, command := model.Update(keyMsg("a"))
	model = updated.(Model)
	if command != nil || model.confirm != "GRANTED" || api.decision != "" {
		t.Fatalf("approval should await confirmation: confirm=%q decision=%q", model.confirm, api.decision)
	}
	if !strings.Contains(model.View(), "Confirm approve?") {
		t.Fatalf("confirmation prompt missing from view")
	}

	updated, command = model.Update(keyMsg("y"))
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed approval did not produce a command")
	}
	message := command()
	if _, ok := message.(decisionMsg); !ok {
		t.Fatalf("command returned %T", message)
	}
	if api.id != "approval-2" || api.decision != "GRANTED" || api.actor != "terminal-operator" {
		t.Fatalf("decision call = id:%q decision:%q actor:%q", api.id, api.decision, api.actor)
	}
}

func TestModelRendersNestedInvestigationTicket(t *testing.T) {
	model := NewModel(&fakeAPI{}, Options{})
	snapshot := Snapshot{Overview: admin.Overview{Status: "ok"}, Investigations: []Investigation{{
		Investigation: domain.Investigation{ID: "inv-123", Status: domain.InvestigationRunning, CreatedAt: time.Now()},
		Ticket:        admin.TicketSummary{ExternalID: "HELP-9", Summary: "Reset locked account"},
	}}, LoadedAt: time.Now()}
	updated, _ := model.Update(snapshotMsg(snapshot))
	model = updated.(Model)
	updated, _ = model.Update(keyMsg("2"))
	model = updated.(Model)
	view := model.View()
	if !strings.Contains(view, "HELP-9") || !strings.Contains(view, "Reset locked account") {
		t.Fatalf("investigation ticket missing from view:\n%s", view)
	}
}

func keyMsg(value string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
}

func TestModelShowsInitialRoutingSeparatelyFromActiveSpecialist(t *testing.T) {
	model := NewModel(&fakeAPI{}, Options{})
	model.width = 140
	model.view = investigationsView
	model.snapshot.Investigations = []Investigation{{
		Investigation: domain.Investigation{ID: "inv-route", Status: domain.InvestigationRunning, Specialist: "access_management", Routing: &domain.RoutingDecision{
			Specialist: "help_desk", Classification: "unknown", Source: "fallback", FallbackCode: "model_low_confidence", Reason: "The ticket needs clarification.",
		}},
	}}
	view := model.View()
	for _, expected := range []string{"access_management", "Initial routing: fallback → help_desk (unknown)", "Fallback: model_low_confidence", "The ticket needs clarification."} {
		if !strings.Contains(view, expected) {
			t.Errorf("routing detail missing %q from view:\n%s", expected, view)
		}
	}
	model.snapshot.Investigations[0].Routing = nil
	if !strings.Contains(model.View(), "Initial routing: not recorded") {
		t.Fatal("legacy investigation must show absent routing data")
	}
}

func TestRoutingRenderingRemovesUntrustedTerminalControls(t *testing.T) {
	view := renderRouting(&domain.RoutingDecision{Specialist: "help_desk", Classification: "help_desk", Source: "rule", RuleName: "rule\nspoofed", Reason: "text\x1b]52;c;payload\a"}, 200)
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\a") || strings.Contains(view, "rule\nspoofed") {
		t.Fatalf("terminal controls survived: %q", view)
	}
}
