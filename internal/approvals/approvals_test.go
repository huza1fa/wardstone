package approvals

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestValidateBindsApprovalToExactAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	action := testAction(t)
	approval := domain.Approval{ActionID: action.ID, ActionDigest: action.Digest, Status: domain.ApprovalGranted, ExpiresAt: now.Add(time.Hour)}
	if err := Validate(approval, action, now); err != nil {
		t.Fatalf("valid approval rejected: %v", err)
	}
	action.Arguments = json.RawMessage(`{"group":"admins@example.com","user":"jane@example.com"}`)
	if err := Validate(approval, action, now); !errors.Is(err, ErrActionChanged) {
		t.Fatalf("expected changed action error, got %v", err)
	}
}

func TestValidateRejectsExpiredApproval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	action := testAction(t)
	approval := domain.Approval{ActionID: action.ID, ActionDigest: action.Digest, Status: domain.ApprovalGranted, ExpiresAt: now}
	if err := Validate(approval, action, now); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected expired error, got %v", err)
	}
}

func testAction(t *testing.T) domain.ProposedAction {
	t.Helper()
	action := domain.ProposedAction{ID: "act_1", InvestigationID: "inv_1", Capability: "google.groups.add_member", Arguments: json.RawMessage(`{"group":"engineering@example.com","user":"jane@example.com"}`)}
	if err := action.Seal(); err != nil {
		t.Fatal(err)
	}
	return action
}
