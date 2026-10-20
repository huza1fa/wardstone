package api_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/specialists"
	"github.com/wardstone-project/wardstone/internal/worker"
)

func operatorCaseServer(t *testing.T, model *pilotModel) (*database.Store, interface {
	Run(context.Context, domain.InvestigationID) error
}, *api.Server) {
	t.Helper()
	pool := routingTestDatabase(t)
	store, service, _ := pilotService(t, pool, model, nil, []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Clarify and report"}}, nil)
	approval, err := approvals.NewService(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.NewServer(service, store, "pilot-webhook", "pilot-operator", api.WithCases(store, "case-controls-test", true), api.WithAdmin(store, approval, domain.OperatingModeReview), api.WithConversations(service), api.WithMessageReviews(store, true))
	if err != nil {
		t.Fatal(err)
	}
	return store, service, server
}
func caseCommand(key string) cases.Command {
	return cases.Command{RequestID: key, Actor: "pilot-operator", Reason: "Synthetic operator test"}
}

func TestOperatorCaseCancellationAndLinkedRetry(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	model := &pilotModel{respond: func(r agent.Request) agent.Result {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return agent.Result{Diagnosis: "Synthetic completed review"}
	}}
	store, service, server := operatorCaseServer(t, model)
	payload := cases.Create{Command: caseCommand("create-case-1"), Summary: "Review a local case", Metadata: domain.TicketMetadata{RequestType: "General question"}}
	var first, duplicate struct {
		ID      domain.InvestigationID `json:"investigation_id"`
		Created bool                   `json:"created"`
	}
	pilotHTTP(t, server, "POST", "/v1/admin/cases", "bad-token", payload, 401, nil)
	pilotHTTP(t, server, "POST", "/v1/admin/cases", "pilot-operator", payload, 202, &first)
	pilotHTTP(t, server, "POST", "/v1/admin/cases", "pilot-operator", payload, 202, &duplicate)
	if first.ID != duplicate.ID || !first.Created || duplicate.Created {
		t.Fatal("creation not idempotent")
	}
	payload.Summary = "Changed input"
	pilotHTTP(t, server, "POST", "/v1/admin/cases", "pilot-operator", payload, 409, nil)
	job, err := store.Claim(context.Background(), "case-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.Run(worker.WithLease(context.Background(), job), first.ID) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("model never entered")
	}
	cancel := caseCommand("cancel-case-1")
	pilotHTTP(t, server, "POST", "/v1/admin/investigations/"+string(first.ID)+"/cancel", "pilot-operator", cancel, 200, nil)
	pilotHTTP(t, server, "POST", "/v1/admin/investigations/"+string(first.ID)+"/cancel", "pilot-operator", cancel, 200, nil)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("late worker committed after cancellation")
	}
	original, err := store.GetInvestigation(context.Background(), first.ID)
	if err != nil || original.Status != domain.InvestigationCancelled || original.Diagnosis != "" {
		t.Fatalf("cancelled=%+v err=%v", original, err)
	}
	if err := store.Renew(context.Background(), job, time.Minute); !errors.Is(err, worker.ErrLeaseLost) {
		t.Fatalf("cancelled lease alive: %v", err)
	}
	var retry, again struct {
		ID      domain.InvestigationID `json:"investigation_id"`
		Created bool                   `json:"created"`
	}
	command := caseCommand("retry-case-1")
	path := "/v1/admin/investigations/" + string(first.ID) + "/retry"
	pilotHTTP(t, server, "POST", path, "pilot-operator", command, 202, &retry)
	pilotHTTP(t, server, "POST", path, "pilot-operator", command, 202, &again)
	if retry.ID == first.ID || retry.ID != again.ID || again.Created {
		t.Fatal("retry did not create one linked attempt")
	}
	pilotHTTP(t, server, "POST", path, "pilot-operator", caseCommand("other-retry-1"), 409, nil)
	child, err := store.GetInvestigation(context.Background(), retry.ID)
	if err != nil || child.ParentID != first.ID || child.CaseID != first.ID {
		t.Fatalf("child=%+v %v", child, err)
	}
	next, err := store.Claim(context.Background(), "case-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(worker.WithLease(context.Background(), next), retry.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if len(model.requests[1].Evidence) != 0 || len(model.requests[1].Conversation) != 0 {
		t.Fatal("retry reused prior evidence/conversation")
	}
	attempts, err := store.CaseAttempts(context.Background(), retry.ID)
	if err != nil || len(attempts) != 2 || attempts[0].Status != domain.InvestigationCancelled || attempts[1].Status != domain.InvestigationCompleted {
		t.Fatalf("history=%+v %v", attempts, err)
	}
	events, _ := store.Timeline(context.Background(), first.ID)
	if !strings.Contains(string(events[len(events)-1].Data), string(retry.ID)) {
		t.Fatal("parent lacks retry audit")
	}
	pilotHTTP(t, server, "POST", "/v1/admin/investigations/"+string(retry.ID)+"/cancel", "pilot-operator", caseCommand("cancel-complete-1"), 409, nil)
}

func TestOperatorCaseQuestionAndReplyRemainInternal(t *testing.T) {
	model := &pilotModel{respond: func(r agent.Request) agent.Result {
		if len(r.Conversation) == 0 {
			return agent.Result{FollowUpQuestion: "Which configured topic?"}
		}
		return agent.Result{Diagnosis: "Synthetic local answer", WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Local review", RequesterSummary: "Local result"}}
	}}
	store, service, server := operatorCaseServer(t, model)
	var result struct {
		ID domain.InvestigationID `json:"investigation_id"`
	}
	pilotHTTP(t, server, "POST", "/v1/admin/cases", "pilot-operator", cases.Create{Command: caseCommand("create-reply-case"), Summary: "Investigate local question"}, 202, &result)
	run := func() {
		job, err := store.Claim(context.Background(), "local-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Run(worker.WithLease(context.Background(), job), result.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.Complete(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	run()
	reply := cases.Reply{Command: caseCommand("reply-local-case"), Body: "VPN troubleshooting"}
	path := "/v1/admin/investigations/" + string(result.ID) + "/reply"
	pilotHTTP(t, server, "POST", path, "pilot-operator", reply, 202, nil)
	pilotHTTP(t, server, "POST", path, "pilot-operator", reply, 202, nil)
	run()
	messages, _ := store.ListMessages(context.Background(), result.ID)
	if len(messages) != 3 || messages[1].Body != reply.Body || messages[1].Author != "operator" {
		t.Fatalf("messages=%+v", messages)
	}
	deliveries, _ := store.ListDeliveries(context.Background(), result.ID)
	if len(deliveries) != 0 {
		t.Fatal("local case enqueued external delivery")
	}
	var detail struct {
		Attempts []domain.Investigation `json:"attempts"`
		Ticket   domain.Ticket          `json:"ticket"`
	}
	pilotHTTP(t, server, "GET", "/v1/admin/investigations/"+string(result.ID)+"/case", "pilot-operator", nil, 200, &detail)
	if detail.Ticket.Source != "operator" || len(detail.Attempts) != 1 {
		t.Fatalf("case detail=%+v", detail)
	}
}

func TestExhaustedWorkersBecomeRetryableEvenWhenQueueIsEmpty(t *testing.T) {
	pool := routingTestDatabase(t)
	store := database.NewStore(pool)
	ctx := context.Background()
	id, _, err := store.CreateCase(ctx, cases.Create{Command: caseCommand("exhausted-case-create"), Summary: "Crash recovery limit"}, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE investigations SET status='RUNNING' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE jobs SET status='RUNNING',attempt=3,lease_owner='dead-worker',lease_expires_at=now()-interval '1 second' WHERE investigation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Claim(ctx, "recovery-worker", time.Minute); !errors.Is(err, worker.ErrNoJob) {
		t.Fatalf("exhausted claim=%v", err)
	}
	item, err := store.GetInvestigation(ctx, id)
	if err != nil || item.Status != domain.InvestigationFailed || item.Failure != "worker_recovery_limit" {
		t.Fatalf("stranded case: %+v %v", item, err)
	}
	child, created, err := store.RetryCase(ctx, id, caseCommand("retry-exhausted-case"), "test", time.Now())
	if err != nil || !created || child == id {
		t.Fatalf("retry=%s %v %v", child, created, err)
	}
}

func TestConcurrentCaseRetriesCreateOneChild(t *testing.T) {
	pool := routingTestDatabase(t)
	store := database.NewStore(pool)
	ctx := context.Background()
	id, _, err := store.CreateCase(ctx, cases.Create{Command: caseCommand("concurrent-create-case"), Summary: "Concurrent operator requests"}, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelCase(ctx, id, caseCommand("concurrent-cancel-case"), time.Now()); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		created bool
		err     error
	}
	results := make(chan outcome, 8)
	start := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			<-start
			_, created, err := database.NewStore(pool).RetryCase(ctx, id, caseCommand(fmt.Sprintf("concurrent-retry-%d", i)), "test", time.Now())
			results <- outcome{created, err}
		}(i)
	}
	close(start)
	created := 0
	for range 8 {
		result := <-results
		if result.err != nil && !errors.Is(result.err, cases.ErrConflict) {
			t.Fatal(result.err)
		}
		if result.created && result.err == nil {
			created++
		}
	}
	attempts, err := store.CaseAttempts(ctx, id)
	if err != nil || created != 1 || len(attempts) != 2 {
		t.Fatalf("retry race created=%d attempts=%+v err=%v", created, attempts, err)
	}
}

func TestCancellationSupersedesUnsentReviewedJiraQuestion(t *testing.T) {
	pool := routingTestDatabase(t)
	model := &pilotModel{respond: func(agent.Request) agent.Result { return agent.Result{FollowUpQuestion: "Which configured system?"} }}
	store, service, server := pilotService(t, pool, model, nil, []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Clarify"}}, nil)
	var intake struct {
		ID domain.InvestigationID `json:"investigation_id"`
	}
	pilotHTTP(t, server, "POST", "/v1/tickets/jira", "pilot-webhook", jira.TicketPayload{ExternalID: "CANCEL-QUESTION", Summary: "Clarify this question", ReporterEmail: "requester@example.test"}, 202, &intake)
	pilotRun(t, store, service, intake.ID)
	messages, err := store.ListMessages(context.Background(), intake.ID)
	if err != nil || len(messages) != 1 {
		t.Fatal(err)
	}
	_, _, err = store.ReviewMessage(context.Background(), intake.ID, messages[0].ID, delivery.Review{Actor: "admin", Body: messages[0].Body, DraftDigest: delivery.DraftDigest(messages[0].Body)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelCase(context.Background(), intake.ID, caseCommand("cancel-reviewed-question"), time.Now()); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.ListDeliveries(context.Background(), intake.ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].Status != domain.DeliverySuperseded {
		t.Fatalf("unsent delivery=%+v %v", deliveries, err)
	}
	if _, err := store.ClaimReviewedDelivery(context.Background(), "sender", time.Minute); !errors.Is(err, delivery.ErrNoDelivery) {
		t.Fatalf("cancelled delivery claimed: %v", err)
	}
}

func TestWorkerLeaseIsRecheckedAfterWaitingForCaseLock(t *testing.T) {
	pool := routingTestDatabase(t)
	store := database.NewStore(pool)
	ctx := context.Background()
	id, _, err := store.CreateCase(ctx, cases.Create{Command: caseCommand("lease-lock-create"), Summary: "Verify stale worker fencing"}, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Claim(ctx, "original-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	runCtx := worker.WithLease(ctx, job)
	if err := store.StartInvestigation(runCtx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err = blocker.Exec(ctx, `SELECT id FROM investigations WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- store.RecordEvidence(runCtx, id, []domain.Evidence{{ID: domain.NewEvidenceID(), Source: "test", Capability: "test.read", Kind: "test.observation", Data: []byte(`{}`), ObservedAt: time.Now()}}, nil, time.Now())
	}()
	// The original writer cannot pass the case lock. Make its lease expire while
	// it waits, and prove a claimant also respects the held case lock.
	select {
	case err := <-done:
		t.Fatalf("writer passed case lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, err = pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Claim(ctx, "competitor", time.Minute); !errors.Is(err, worker.ErrNoJob) {
		t.Fatalf("claim bypassed held case lock: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, worker.ErrLeaseLost) {
		t.Fatalf("stale writer was not fenced: %v", err)
	}
	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE investigation_id=$1`, id).Scan(&count)
	if count != 0 {
		t.Fatal("stale evidence committed")
	}
	next, err := store.Claim(ctx, "new-worker", time.Minute)
	if err != nil || next.Attempt != 2 {
		t.Fatalf("lease takeover failed: %+v %v", next, err)
	}
}
