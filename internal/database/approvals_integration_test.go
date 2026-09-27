package database

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	approvalstate "github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestApprovalLifecyclePostgres(t *testing.T) {
	databaseURL := os.Getenv("WARDSTONE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WARDSTONE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := NewStore(pool)

	now := time.Now().UTC()
	ticketID := domain.NewTicketID()
	investigationID := domain.NewInvestigationID()
	action := domain.ProposedAction{
		ID: domain.NewActionID(), InvestigationID: investigationID,
		Capability: "google.groups.add_member",
		Arguments:  json.RawMessage(`{"group":"engineering@example.com","user":"jane@example.com"}`),
		Reason:     "Grant requested access", EvidenceIDs: []domain.EvidenceID{"evd_test"}, CreatedAt: now,
	}
	if err := action.Seal(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets
		(id, source, external_id, summary, description, reporter_email, created_at)
		VALUES ($1, 'jira', $2, 'Access request', '', '', $3)`, ticketID, "TEST-"+string(ticketID), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO investigations
		(id, ticket_id, status, prompt_version, created_at, completed_at)
		VALUES ($1, $2, 'COMPLETED', 'test-v1', $3, $3)`, investigationID, ticketID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO proposed_actions
		(id, investigation_id, capability, arguments, reason, evidence_ids, action_digest,
		 policy_decision, policy_reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'REQUIRE_APPROVAL', 'test', $8)`,
		action.ID, action.InvestigationID, action.Capability, action.Arguments, action.Reason,
		[]byte(`[]`), action.Digest, action.CreatedAt); err != nil {
		t.Fatal(err)
	}

	service, err := approvalstate.NewService(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 12
	type requestResult struct {
		approval domain.Approval
		created  bool
		err      error
	}
	requests := make(chan requestResult, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			approval, created, err := service.Request(ctx, action)
			requests <- requestResult{approval: approval, created: created, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(requests)

	var approvalID domain.ApprovalID
	createdCount := 0
	for result := range requests {
		if result.err != nil {
			t.Fatalf("concurrent request failed: %v", result.err)
		}
		if result.created {
			createdCount++
		}
		if approvalID == "" {
			approvalID = result.approval.ID
		}
		if result.approval.ID != approvalID {
			t.Fatalf("request returned a second active approval: %s != %s", result.approval.ID, approvalID)
		}
	}
	if createdCount != 1 {
		t.Fatalf("expected one created approval, got %d", createdCount)
	}

	decisions := make(chan error, callers)
	start = make(chan struct{})
	for i := range callers {
		decision := domain.ApprovalGranted
		if i%2 == 1 {
			decision = domain.ApprovalDenied
		}
		group.Add(1)
		go func(decision domain.ApprovalStatus) {
			defer group.Done()
			<-start
			_, err := service.Decide(ctx, approvalID, decision, "integration-test@example.com")
			decisions <- err
		}(decision)
	}
	close(start)
	group.Wait()
	close(decisions)
	winners := 0
	for err := range decisions {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, approvalstate.ErrAlreadyDecided) {
			t.Fatalf("unexpected decision error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected one decision winner, got %d", winners)
	}

	if _, err := pool.Exec(ctx, `UPDATE proposed_actions SET action_digest = 'tampered' WHERE id = $1`, action.ID); err == nil {
		t.Fatal("database allowed a proposed action digest to change")
	}
	if _, err := pool.Exec(ctx, `UPDATE approvals SET action_digest = 'tampered' WHERE id = $1`, approvalID); err == nil {
		t.Fatal("database allowed an approval binding to change")
	}
	timeline, err := store.Timeline(ctx, investigationID)
	if err != nil {
		t.Fatal(err)
	}
	requested, decided := 0, 0
	for _, event := range timeline {
		switch event.Type {
		case audit.ApprovalRequested:
			requested++
		case audit.ApprovalGranted, audit.ApprovalDenied:
			decided++
		}
	}
	if requested != 1 || decided != 1 {
		t.Fatalf("unexpected audit counts: requested=%d decided=%d", requested, decided)
	}
}
