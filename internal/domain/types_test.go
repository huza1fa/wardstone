package domain

import (
	"encoding/json"
	"testing"
)

func TestProposedActionSealCanonicalizesArguments(t *testing.T) {
	t.Parallel()
	first := ProposedAction{ID: "act_1", InvestigationID: "inv_1", Capability: "google.groups.add_member", Arguments: json.RawMessage(`{"user":"jane@example.com","group":"engineering@example.com"}`)}
	second := ProposedAction{ID: "act_2", InvestigationID: "inv_1", Capability: "google.groups.add_member", Arguments: json.RawMessage(`{ "group": "engineering@example.com", "user": "jane@example.com" }`)}
	if err := first.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := second.Seal(); err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("equivalent arguments produced different digests: %s != %s", first.Digest, second.Digest)
	}
	if string(first.Arguments) != `{"group":"engineering@example.com","user":"jane@example.com"}` {
		t.Fatalf("arguments were not canonicalized: %s", first.Arguments)
	}
}

func TestProposedActionDigestDetectsChangedArguments(t *testing.T) {
	t.Parallel()
	action := ProposedAction{ID: "act_1", InvestigationID: "inv_1", Capability: "google.groups.add_member", Arguments: json.RawMessage(`{"group":"engineering@example.com","user":"jane@example.com"}`)}
	if err := action.Seal(); err != nil {
		t.Fatal(err)
	}
	action.Arguments = json.RawMessage(`{"group":"admins@example.com","user":"jane@example.com"}`)
	if action.DigestValid() {
		t.Fatal("changed arguments retained a valid digest")
	}
}

func TestProposedActionRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	action := ProposedAction{ID: "act_1", InvestigationID: "inv_1", Capability: "test", Arguments: json.RawMessage(`{} {}`)}
	if err := action.Seal(); err == nil {
		t.Fatal("expected trailing JSON to be rejected")
	}
}
