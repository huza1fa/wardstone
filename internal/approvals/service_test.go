package approvals_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/investigations/memory"
)

func TestApprovalLifecycleBindsDecisionToExactAction(t *testing.T) {
	t.Parallel()
	service, store, action, clock := approvalFixture(t, domain.PolicyRequireApproval)

	approval, created, err := service.Request(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if !created || approval.Status != domain.ApprovalPending {
		t.Fatalf("unexpected request result: created=%v approval=%+v", created, approval)
	}
	if !approval.ExpiresAt.Equal(clock.now.Add(time.Hour)) {
		t.Fatalf("unexpected expiry: %v", approval.ExpiresAt)
	}

	duplicate, created, err := service.Request(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if created || duplicate.ID != approval.ID {
		t.Fatalf("duplicate request created another approval: created=%v id=%s", created, duplicate.ID)
	}

	clock.now = clock.now.Add(10 * time.Minute)
	granted, err := service.Decide(context.Background(), approval.ID, domain.ApprovalGranted, "operator@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := approvals.Validate(granted, action, clock.now); err != nil {
		t.Fatalf("valid granted approval rejected: %v", err)
	}

	timeline, err := store.Timeline(context.Background(), action.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvents(timeline, audit.ApprovalRequested) != 1 || countEvents(timeline, audit.ApprovalGranted) != 1 {
		t.Fatalf("unexpected approval audit events: %+v", timeline)
	}
	last := timeline[len(timeline)-1]
	if last.ActorType != audit.ActorOperator || last.ActorID != "operator@example.com" {
		t.Fatalf("decision actor was not audited: %+v", last)
	}
}

func TestApprovalRequestRejectsTamperedOrIneligibleAction(t *testing.T) {
	t.Parallel()
	t.Run("tampered arguments", func(t *testing.T) {
		service, _, action, _ := approvalFixture(t, domain.PolicyRequireApproval)
		action.Arguments = json.RawMessage(`{"group":"admins@example.com","user":"jane@example.com"}`)
		if _, _, err := service.Request(context.Background(), action); !errors.Is(err, approvals.ErrActionChanged) {
			t.Fatalf("expected changed action error, got %v", err)
		}
	})

	t.Run("different valid digest", func(t *testing.T) {
		service, _, action, _ := approvalFixture(t, domain.PolicyRequireApproval)
		action.Arguments = json.RawMessage(`{"group":"admins@example.com","user":"jane@example.com"}`)
		if err := action.Seal(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.Request(context.Background(), action); !errors.Is(err, approvals.ErrActionChanged) {
			t.Fatalf("expected changed action error, got %v", err)
		}
	})

	t.Run("policy does not require approval", func(t *testing.T) {
		service, _, action, _ := approvalFixture(t, domain.PolicyAllow)
		if _, _, err := service.Request(context.Background(), action); !errors.Is(err, approvals.ErrActionNotEligible) {
			t.Fatalf("expected ineligible action error, got %v", err)
		}
	})
}

func TestApprovalDecisionExpiresAtBoundary(t *testing.T) {
	t.Parallel()
	service, store, action, clock := approvalFixture(t, domain.PolicyRequireApproval)
	approval, _, err := service.Request(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = approval.ExpiresAt

	expired, err := service.Decide(context.Background(), approval.ID, domain.ApprovalGranted, "operator@example.com")
	if !errors.Is(err, approvals.ErrExpired) {
		t.Fatalf("expected expired error, got %v", err)
	}
	if expired.Status != domain.ApprovalExpired || expired.Actor != "" || expired.DecidedAt != nil {
		t.Fatalf("unexpected expired approval: %+v", expired)
	}
	persisted, err := service.Get(context.Background(), approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != domain.ApprovalExpired {
		t.Fatalf("expiry was not persisted: %+v", persisted)
	}
	timeline, err := store.Timeline(context.Background(), action.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvents(timeline, audit.ApprovalExpired) != 1 || countEvents(timeline, audit.ApprovalGranted) != 0 {
		t.Fatalf("unexpected expiry audit events: %+v", timeline)
	}
}

func TestExpiredActiveApprovalCanBeRequestedAgain(t *testing.T) {
	t.Parallel()
	service, store, action, clock := approvalFixture(t, domain.PolicyRequireApproval)
	first, _, err := service.Request(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = first.ExpiresAt
	second, created, err := service.Request(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if !created || second.ID == first.ID {
		t.Fatalf("expired approval was not replaced: created=%v first=%s second=%s", created, first.ID, second.ID)
	}
	expired, err := service.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Status != domain.ApprovalExpired {
		t.Fatalf("old approval was not expired: %+v", expired)
	}
	timeline, err := store.Timeline(context.Background(), action.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvents(timeline, audit.ApprovalRequested) != 2 || countEvents(timeline, audit.ApprovalExpired) != 1 {
		t.Fatalf("unexpected replacement audit events: %+v", timeline)
	}
}

func TestConcurrentApprovalRequestsCreateOneActiveApproval(t *testing.T) {
	t.Parallel()
	service, store, action, _ := approvalFixture(t, domain.PolicyRequireApproval)
	const callers = 32
	type result struct {
		approval domain.Approval
		created  bool
		err      error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			approval, created, err := service.Request(context.Background(), action)
			results <- result{approval: approval, created: created, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	var approvalID domain.ApprovalID
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("request failed: %v", result.err)
		}
		if result.created {
			createdCount++
		}
		if approvalID == "" {
			approvalID = result.approval.ID
		}
		if result.approval.ID != approvalID {
			t.Fatalf("requests returned different approvals: %s != %s", result.approval.ID, approvalID)
		}
	}
	if createdCount != 1 {
		t.Fatalf("expected one created approval, got %d", createdCount)
	}
	timeline, err := store.Timeline(context.Background(), action.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvents(timeline, audit.ApprovalRequested) != 1 {
		t.Fatalf("expected one request event, got %+v", timeline)
	}
}

func TestConcurrentApprovalDecisionsHaveOneWinner(t *testing.T) {
	t.Parallel()
	service, store, action, _ := approvalFixture(t, domain.PolicyRequireApproval)
	approval, _, err := service.Request(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 32
	errorsByCaller := make(chan error, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range callers {
		decision := domain.ApprovalGranted
		if i%2 == 1 {
			decision = domain.ApprovalDenied
		}
		group.Add(1)
		go func(index int, decision domain.ApprovalStatus) {
			defer group.Done()
			<-start
			_, err := service.Decide(context.Background(), approval.ID, decision, "operator@example.com")
			errorsByCaller <- err
		}(i, decision)
	}
	close(start)
	group.Wait()
	close(errorsByCaller)

	winners := 0
	for err := range errorsByCaller {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, approvals.ErrAlreadyDecided) {
			t.Fatalf("unexpected decision error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected one winning decision, got %d", winners)
	}
	timeline, err := store.Timeline(context.Background(), action.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	decisions := countEvents(timeline, audit.ApprovalGranted) + countEvents(timeline, audit.ApprovalDenied)
	if decisions != 1 {
		t.Fatalf("expected one decision event, got %d", decisions)
	}
}

func TestApprovalDecisionValidatesActorAndDecision(t *testing.T) {
	t.Parallel()
	service, _, action, _ := approvalFixture(t, domain.PolicyRequireApproval)
	approval, _, err := service.Request(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(context.Background(), approval.ID, domain.ApprovalPending, "operator@example.com"); !errors.Is(err, approvals.ErrInvalidDecision) {
		t.Fatalf("expected invalid decision error, got %v", err)
	}
	if _, err := service.Decide(context.Background(), approval.ID, domain.ApprovalGranted, "  "); !errors.Is(err, approvals.ErrInvalidActor) {
		t.Fatalf("expected invalid actor error, got %v", err)
	}
}

type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Now() time.Time { return c.now }

func approvalFixture(t *testing.T, policyDecision domain.PolicyDecision) (*approvals.Service, *memory.Store, domain.ProposedAction, *fixedClock) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	store := memory.NewStore()
	ticket := domain.Ticket{
		ID: "tkt_1", Source: "jira", ExternalID: "HELP-1", Summary: "Access request", CreatedAt: now,
	}
	investigation := domain.Investigation{
		ID: "inv_1", TicketID: ticket.ID, Status: domain.InvestigationPending, PromptVersion: "test-v1", CreatedAt: now,
	}
	if _, _, err := store.ReceiveTicket(ctx, ticket, investigation); err != nil {
		t.Fatal(err)
	}
	if err := store.StartInvestigation(ctx, investigation.ID, now); err != nil {
		t.Fatal(err)
	}
	action := domain.ProposedAction{
		ID: "act_1", InvestigationID: investigation.ID, Capability: "google.groups.add_member",
		Arguments: json.RawMessage(`{"group":"engineering@example.com","user":"jane@example.com"}`),
		Reason:    "Grant the requested access", EvidenceIDs: []domain.EvidenceID{"evd_1"}, CreatedAt: now,
	}
	if err := action.Seal(); err != nil {
		t.Fatal(err)
	}
	evaluation := investigations.ActionEvaluation{
		Action: action, Policy: domain.PolicyResult{Decision: policyDecision, Reason: "test"},
	}
	if err := store.CompleteInvestigation(ctx, investigation.ID, "Test diagnosis", "fake", "fake-v1", []investigations.ActionEvaluation{evaluation}, nil, now); err != nil {
		t.Fatal(err)
	}
	service, err := approvals.NewService(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{now: now.Add(time.Minute)}
	service.SetClockForTest(clock)
	return service, store, action, clock
}

func countEvents(events []audit.Event, eventType audit.EventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}
