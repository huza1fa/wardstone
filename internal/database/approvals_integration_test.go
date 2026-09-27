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
	"github.com/wardstone-project/wardstone/internal/investigations"
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

func TestRequesterConversationPostgres(t *testing.T) {
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
	ticketID, investigationID := domain.NewTicketID(), domain.NewInvestigationID()
	if _, err := pool.Exec(ctx, `INSERT INTO tickets
		(id, source, external_id, summary, description, reporter_email, created_at)
		VALUES ($1, 'jira', $2, 'VPN issue', '', '', $3)`, ticketID, "TEST-"+string(ticketID), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO investigations (id, ticket_id, status, prompt_version, created_at)
		VALUES ($1, $2, 'PENDING', 'test-v1', $3)`, investigationID, ticketID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.StartInvestigation(ctx, investigationID, now); err != nil {
		t.Fatal(err)
	}
	question := domain.CaseMessage{ID: domain.NewMessageID(), Source: "jira", ExternalID: "wardstone-question-" + string(investigationID), Direction: domain.MessageOutbound, Body: "Which VPN error do you see?", CreatedAt: now}
	if err := store.WaitForRequester(ctx, investigationID, question, now); err != nil {
		t.Fatal(err)
	}
	var deliveryStatus domain.DeliveryStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM message_deliveries WHERE message_id = $1`, question.ID).Scan(&deliveryStatus); err != nil || deliveryStatus != domain.DeliveryPending {
		t.Fatalf("question delivery status = %q, err=%v", deliveryStatus, err)
	}

	const callers = 12
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, created, err := store.ReceiveRequesterReply(ctx, "jira", "TEST-"+string(ticketID), domain.CaseMessage{
				ID: domain.NewMessageID(), Source: "jira", ExternalID: "comment-" + string(investigationID), Direction: domain.MessageInbound, Body: "Error 691", CreatedAt: now,
			}, now)
			results <- result{created: created, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created replies = %d, want 1", createdCount)
	}
	item, err := store.GetInvestigation(ctx, investigationID)
	if err != nil || item.Status != domain.InvestigationPending {
		t.Fatalf("investigation = %+v, err=%v", item, err)
	}
	messages, err := store.ListMessages(ctx, investigationID)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages = %+v, err=%v", messages, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_messages SET body = 'tampered' WHERE id = $1`, question.ID); err == nil {
		t.Fatal("database allowed a case message to change")
	}
	job, err := store.Claim(ctx, "conversation-test", time.Minute)
	if err != nil || job.InvestigationID != investigationID || job.Kind != "investigate" {
		t.Fatalf("resumed job = %+v, err=%v", job, err)
	}
	if err := store.Complete(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReceiveRequesterReply(ctx, "jira", "TEST-"+string(ticketID), domain.CaseMessage{
		ID: domain.NewMessageID(), Source: "jira", ExternalID: "new-comment-" + string(investigationID), Direction: domain.MessageInbound, Body: "Another detail", CreatedAt: now,
	}, now); !errors.Is(err, investigations.ErrNotAwaitingReply) {
		t.Fatalf("second distinct reply error = %v, want %v", err, investigations.ErrNotAwaitingReply)
	}
}
