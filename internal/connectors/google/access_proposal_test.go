package google

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func proposalInspection() AccessEvidenceV1 {
	return AccessEvidenceV1{
		Version: 1, Status: "observed", Resources: map[string]string{"Engineering tools": "tools@example.com"},
		Resource: "Engineering tools", ResourceSource: "ticket_field", GroupEmail: "tools@example.com",
		UserEmail: "target@example.com", UserSource: "reporter", Unknowns: []AccessUnknown{}, Clarifications: []string{}, Escalations: []string{},
		User:  &User{ID: "user123", PrimaryEmail: "target@example.com", Suspended: false},
		Group: &Group{ID: "group123", Email: "tools@example.com"}, DirectMembership: "not_member",
	}
}

func proposalEvidence(t *testing.T, id domain.EvidenceID, inspection AccessEvidenceV1) domain.Evidence {
	t.Helper()
	evidence, err := accessEvidence(inspection)
	if err != nil {
		t.Fatal(err)
	}
	evidence[0].ID = id
	evidence[0].Capability = AccessInspect // Core-owned provenance tagging.
	return evidence[0]
}

func addMemberProposal() domain.ProposedAction {
	return domain.ProposedAction{Capability: GroupsAddMember, Arguments: json.RawMessage(`{"group":"tools@example.com","user":"target@example.com"}`), EvidenceIDs: []domain.EvidenceID{"access-1"}}
}

func TestAccessProposalRequiresBoundInspection(t *testing.T) {
	t.Parallel()
	collector, err := NewAccessCollector(&HTTPClient{}, map[string]string{"Engineering tools": "tools@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		modify func(*AccessEvidenceV1)
	}{
		{"suspended", func(d *AccessEvidenceV1) { d.User.Suspended = true }},
		{"already member", func(d *AccessEvidenceV1) {
			d.DirectMembership = "member"
			d.Member = &Member{ID: "user123", Email: d.UserEmail, Type: "USER", Role: "MEMBER"}
		}},
		{"member object contradicts absence", func(d *AccessEvidenceV1) {
			d.Member = &Member{ID: "user123", Email: d.UserEmail, Type: "USER", Role: "MEMBER"}
		}},
		{"membership unknown", func(d *AccessEvidenceV1) { d.DirectMembership = "unknown" }},
		{"unknowns", func(d *AccessEvidenceV1) {
			d.Unknowns = []AccessUnknown{{Field: "user", Reason: "secret provider body"}}
		}},
		{"clarification", func(d *AccessEvidenceV1) { d.Clarifications = []string{"secret requester content"} }},
		{"escalation", func(d *AccessEvidenceV1) { d.Escalations = []string{"secret operator correction"} }},
		{"operator intervention status", func(d *AccessEvidenceV1) { d.Status = "operator_intervention_required" }},
		{"incomplete status", func(d *AccessEvidenceV1) { d.Status = "incomplete_observation" }},
		{"clarification status", func(d *AccessEvidenceV1) { d.Status = "clarification_required" }},
		{"wrong version", func(d *AccessEvidenceV1) { d.Version = 2 }},
		{"missing explicit target", func(d *AccessEvidenceV1) { d.UserEmail = "" }},
		{"wrong target", func(d *AccessEvidenceV1) { d.UserEmail = "other@example.com"; d.User.PrimaryEmail = d.UserEmail }},
		{"wrong principal email", func(d *AccessEvidenceV1) { d.User.PrimaryEmail = "other@example.com" }},
		{"unsafe principal id", func(d *AccessEvidenceV1) { d.User.ID = "../secret" }},
		{"missing user", func(d *AccessEvidenceV1) { d.User = nil }},
		{"missing group", func(d *AccessEvidenceV1) { d.Group = nil }},
		{"wrong group email", func(d *AccessEvidenceV1) { d.Group.Email = "other@example.com" }},
		{"unsafe group id", func(d *AccessEvidenceV1) { d.Group.ID = "group?secret=true" }},
		{"resource tamper", func(d *AccessEvidenceV1) { d.Resource = "Unconfigured resource" }},
		{"group selector tamper", func(d *AccessEvidenceV1) { d.GroupEmail = "other@example.com"; d.Group.Email = d.GroupEmail }},
		{"mapping tamper", func(d *AccessEvidenceV1) { d.Resources[d.Resource] = "other@example.com" }},
		{"missing mapping", func(d *AccessEvidenceV1) { d.Resources = nil }},
		{"extra mapping", func(d *AccessEvidenceV1) { d.Resources["Extra resource"] = "other@example.com" }},
		{"missing unknowns", func(d *AccessEvidenceV1) { d.Unknowns = nil }},
		{"missing clarifications", func(d *AccessEvidenceV1) { d.Clarifications = nil }},
		{"model resource source", func(d *AccessEvidenceV1) { d.ResourceSource = "model" }},
		{"model target source", func(d *AccessEvidenceV1) { d.UserSource = "model" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspection := proposalInspection()
			tc.modify(&inspection)
			err := collector.ValidateProposal(addMemberProposal(), []domain.Evidence{proposalEvidence(t, "access-1", inspection)})
			if err != errAccessInspection {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "@") {
				t.Fatalf("unsanitized error = %v", err)
			}
		})
	}
}

func TestAccessProposalNeedsCorrectAccessCitation(t *testing.T) {
	t.Parallel()
	collector, _ := NewAccessCollector(&HTTPClient{}, map[string]string{"Engineering tools": "tools@example.com"})
	for _, tc := range []struct {
		name   string
		modify func(*domain.Evidence, *domain.ProposedAction)
	}{
		{"uncited inspection", func(e *domain.Evidence, a *domain.ProposedAction) { a.EvidenceIDs = nil }},
		{"citation unavailable", func(e *domain.Evidence, a *domain.ProposedAction) { a.EvidenceIDs = []domain.EvidenceID{"missing"} }},
		{"generic user record", func(e *domain.Evidence, a *domain.ProposedAction) { e.Kind = "google.user"; e.Capability = UsersGet }},
		{"wrong source", func(e *domain.Evidence, a *domain.ProposedAction) { e.Source = "jira" }},
		{"wrong kind", func(e *domain.Evidence, a *domain.ProposedAction) { e.Kind = "google.groups" }},
		{"untagged capability", func(e *domain.Evidence, a *domain.ProposedAction) { e.Capability = "" }},
		{"wrong capability", func(e *domain.Evidence, a *domain.ProposedAction) { e.Capability = UsersGet }},
		{"empty id", func(e *domain.Evidence, a *domain.ProposedAction) { e.ID = ""; a.EvidenceIDs = []domain.EvidenceID{""} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := proposalEvidence(t, "access-1", proposalInspection())
			action := addMemberProposal()
			tc.modify(&evidence, &action)
			if err := collector.ValidateProposal(action, []domain.Evidence{evidence}); err != errAccessCitation {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if err := collector.ValidateProposal(addMemberProposal(), nil); err != errAccessCitation {
		t.Fatalf("missing evidence error = %v", err)
	}
}

func TestAccessProposalConflictingCitationsFailClosed(t *testing.T) {
	t.Parallel()
	collector, _ := NewAccessCollector(&HTTPClient{}, map[string]string{"Engineering tools": "tools@example.com"})
	for _, tc := range []struct {
		name   string
		modify func(*AccessEvidenceV1)
		want   error
	}{
		{"different immutable user", func(d *AccessEvidenceV1) { d.User.ID = "user456" }, errAccessConflict},
		{"different immutable group", func(d *AccessEvidenceV1) { d.Group.ID = "group456" }, errAccessConflict},
		{"conflicting unknown", func(d *AccessEvidenceV1) { d.DirectMembership = "unknown" }, errAccessInspection},
		{"conflicting member", func(d *AccessEvidenceV1) { d.DirectMembership = "member" }, errAccessInspection},
		{"conflicting target", func(d *AccessEvidenceV1) { d.UserEmail = "other@example.com"; d.User.PrimaryEmail = d.UserEmail }, errAccessInspection},
		{"conflicting escalation", func(d *AccessEvidenceV1) { d.Escalations = []string{"Operator must submit a corrected new case."} }, errAccessInspection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := proposalInspection()
			tc.modify(&other)
			first := proposalEvidence(t, "access-1", proposalInspection())
			second := proposalEvidence(t, "access-2", other)
			action := addMemberProposal()
			action.EvidenceIDs = append(action.EvidenceIDs, second.ID)
			for _, evidence := range [][]domain.Evidence{{first, second}, {second, first}} {
				if err := collector.ValidateProposal(action, evidence); err != tc.want {
					t.Fatalf("error = %v, want %v", err, tc.want)
				}
			}
		})
	}
	evidence := proposalEvidence(t, "access-1", proposalInspection())
	if err := collector.ValidateProposal(addMemberProposal(), []domain.Evidence{evidence, evidence}); err != errAccessConflict {
		t.Fatalf("ambiguous citation error = %v", err)
	}
}

func TestAccessProposalArgumentsParsedDefensively(t *testing.T) {
	t.Parallel()
	collector, _ := NewAccessCollector(&HTTPClient{}, map[string]string{"Engineering tools": "tools@example.com"})
	evidence := []domain.Evidence{proposalEvidence(t, "access-1", proposalInspection())}
	for _, input := range []string{
		`null`, `{}`, `{} {}`, `{"group":"tools@example.com","user":"Target <target@example.com>"}`,
		`{"group":"tools@example.com?secret=true","user":"target@example.com"}`,
		`{"group":"tools@example.com","user":"target@example.com","role":"OWNER"}`,
		`{"group":"other@example.com","group":"tools@example.com","user":"target@example.com"}`,
		`{"group":"tools@example.com","user":null}`, strings.Repeat(" ", 4097) + `{}`,
	} {
		action := addMemberProposal()
		action.Arguments = json.RawMessage(input)
		if err := collector.ValidateProposal(action, evidence); err != errAccessArguments {
			t.Fatalf("arguments error = %v", err)
		}
	}
	for _, input := range []string{`{"group":"other@example.com","user":"target@example.com"}`, `{"group":"tools@example.com","user":"other@example.com"}`} {
		action := addMemberProposal()
		action.Arguments = json.RawMessage(input)
		if err := collector.ValidateProposal(action, evidence); err != errAccessInspection {
			t.Fatalf("wrong target error = %v", err)
		}
	}
}

func TestAccessProposalMalformedEvidenceIsSanitized(t *testing.T) {
	t.Parallel()
	collector, _ := NewAccessCollector(&HTTPClient{}, map[string]string{"Engineering tools": "tools@example.com"})
	valid := proposalEvidence(t, "access-1", proposalInspection())
	for _, data := range []string{
		`null`, `{"secret body":`, string(valid.Data) + ` {}`,
		strings.Replace(string(valid.Data), `"suspended":false`, `"suspended":null`, 1),
		strings.Replace(string(valid.Data), `"suspended":false`, `"full_name":"secret body"`, 1),
		strings.Replace(string(valid.Data), `"id":"user123"`, `"id":"wrong123","id":"user123"`, 1),
		strings.Replace(string(valid.Data), `"id":"group123"`, `"id":"wrong123","id":"group123"`, 1),
		strings.Replace(string(valid.Data), `"escalation_requirements":[]`, `"escalation_requirements":null`, 1),
		strings.Replace(string(valid.Data), `"escalation_requirements":[],`, ``, 1),
		strings.Replace(string(valid.Data), `"escalation_requirements":[]`, `"escalation_requirements":"Operator must submit a corrected new case."`, 1),
		strings.Repeat(" ", (128<<10)+1) + `{}`,
	} {
		evidence := valid
		evidence.Data = json.RawMessage(data)
		if err := collector.ValidateProposal(addMemberProposal(), []domain.Evidence{evidence}); err != errAccessInspection {
			t.Fatalf("malformed evidence error = %v", err)
		}
	}
}

func TestAccessProposalValidAddAndOtherCapabilities(t *testing.T) {
	t.Parallel()
	resources := map[string]string{"Engineering tools": "tools@example.com"}
	collector, _ := NewAccessCollector(&HTTPClient{}, resources)
	resources["Engineering tools"] = "tampered@example.com"
	first := proposalEvidence(t, "access-1", proposalInspection())
	action := addMemberProposal()
	if err := collector.ValidateProposal(action, []domain.Evidence{first}); err != nil {
		t.Fatal(err)
	}
	action.Arguments = json.RawMessage(`{"group":"TOOLS@EXAMPLE.COM","user":"TARGET@EXAMPLE.COM"}`)
	secondData := proposalInspection()
	secondData.UserSource = "ticket_field"
	secondData.ResourceSource = "requester_message"
	second := proposalEvidence(t, "access-2", secondData)
	action.EvidenceIDs = append(action.EvidenceIDs, second.ID)
	if err := collector.ValidateProposal(action, []domain.Evidence{first, second}); err != nil {
		t.Fatalf("equivalent complete inspections: %v", err)
	}
	for _, capability := range []domain.CapabilityName{GroupsRemoveMember, GroupsGet, UsersGet, "jira.comment"} {
		action := domain.ProposedAction{Capability: capability}
		err := collector.ValidateProposal(action, nil)
		if capability == GroupsRemoveMember {
			if err != errAccessRemoval {
				t.Fatalf("removal error = %v", err)
			}
		} else if err != nil {
			t.Fatalf("unrelated capability error = %v", err)
		}
	}
}

func TestAccessProposalAcceptsActualCollectorMissingMemberObservation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := directoryFixture(t, &calls, "", "", "ERROR", 404, "target@example.com")
	defer server.Close()
	client, _ := NewHTTPClient(server.URL, "secret", server.Client())
	collector, _ := NewAccessCollector(client, map[string]string{"Engineering tools": "tools@example.com"})
	ticket := accessTicket()
	ticket.Metadata.Fields["Access user"] = []string{"target@example.com"}
	evidence, err := collector.Collect(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	evidence[0].ID = "access-1"
	evidence[0].Capability = AccessInspect
	if err := collector.ValidateProposal(addMemberProposal(), evidence); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("validator performed external calls: %d", calls.Load())
	}
}
