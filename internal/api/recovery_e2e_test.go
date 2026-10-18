package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

type recoveryFixture struct {
	pool        *pgxpool.Pool
	store       *database.Store
	service     *investigations.Service
	server      *api.Server
	recovery    *delivery.RecoveryService
	client      *jira.Client
	id          domain.InvestigationID
	draft       domain.CaseMessage
	first       delivery.Job
	path        string
	reads       atomic.Int32
	writes      atomic.Int32
	blockRead   chan struct{}
	readStarted chan struct{}
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := &recoveryFixture{pool: routingTestDatabase(t)}
	model := &pilotModel{respond: func(agent.Request) agent.Result { return agent.Result{FollowUpQuestion: "Original question?"} }}
	f.store, f.service, f.server = pilotService(t, f.pool, model, nil, []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Clarify"}}, nil)
	var intake struct {
		ID domain.InvestigationID `json:"investigation_id"`
	}
	pilotHTTP(t, f.server, "POST", "/v1/tickets/jira", "pilot-webhook", jira.TicketPayload{ExternalID: "RECOVERY-1", Summary: "Help", ReporterEmail: "requester@example.test"}, 202, &intake)
	f.id = intake.ID
	pilotRun(t, f.store, f.service, f.id)
	messages, _ := f.store.ListMessages(context.Background(), f.id)
	f.draft = messages[0]
	_, _, err := f.store.ReviewMessage(context.Background(), f.id, f.draft.ID, delivery.Review{Body: "Operator-edited question?", Actor: "original-operator", DraftDigest: delivery.DraftDigest(f.draft.Body)}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.first, err = f.store.ClaimReviewedDelivery(context.Background(), "crashed-sender", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE message_deliveries SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, f.first.Delivery.ID); err != nil {
		t.Fatal(err)
	}
	f.store = database.NewStore(f.pool)
	if _, err := f.store.ClaimReviewedDelivery(context.Background(), "restarted-sender", time.Minute); !errors.Is(err, delivery.ErrNoDelivery) {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			f.writes.Add(1)
			t.Errorf("recovery made remote write")
			w.WriteHeader(500)
			return
		}
		f.reads.Add(1)
		switch r.URL.Path {
		case "/rest/api/3/myself":
			w.Write([]byte(`{"accountId":"bot"}`))
			return
		case "/rest/api/3/issue/RECOVERY-1/comment/absent":
			w.WriteHeader(404)
			return
		case "/rest/api/3/issue/RECOVERY-1/comment/unavailable":
			w.WriteHeader(503)
			w.Write([]byte("secret diagnostic"))
			return
		case "/rest/api/3/issue/RECOVERY-1/comment/10001":
			if f.blockRead != nil {
				close(f.readStarted)
				<-f.blockRead
			}
			json.NewEncoder(w).Encode(map[string]any{"properties": []any{map[string]any{"key": "wardstone.message", "value": map[string]string{"message_id": string(f.draft.ID), "body_sha256": delivery.DraftDigest("Operator-edited question?")}}}, "id": "10001", "author": map[string]string{"accountId": "bot"}, "created": time.Now().UTC().Format(time.RFC3339Nano), "body": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]string{"type": "text", "text": "Operator-edited question?"}}}}}})
		default:
			t.Errorf("unexpected remote path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(remote.Close)
	f.client, err = jira.NewClient(remote.URL, "bot@example.test", "test-token", remote.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.rebuild(t)
	f.path = "/v1/investigations/" + string(f.id) + "/messages/" + string(f.draft.ID) + "/recovery"
	return f
}

func (f *recoveryFixture) rebuild(t *testing.T) {
	t.Helper()
	f.store = database.NewStore(f.pool)
	var err error
	f.recovery, err = delivery.NewRecoveryService(f.store, f.client)
	if err != nil {
		t.Fatal(err)
	}
	approval, _ := approvals.NewService(f.store, time.Hour)
	f.server, err = api.NewServer(nil, f.store, "pilot-webhook", "pilot-operator", api.WithAdmin(f.store, approval, domain.OperatingModeReview), api.WithConversations(f.service), api.WithMessageReviews(f.store, true), api.WithMessageRecovery(f.recovery))
	if err != nil {
		t.Fatal(err)
	}
}

func (f *recoveryFixture) item(t *testing.T) domain.MessageDelivery {
	t.Helper()
	items, err := f.store.ListDeliveries(context.Background(), f.id)
	if err != nil || len(items) != 1 {
		t.Fatalf("deliveries=%+v err=%v", items, err)
	}
	return items[0]
}

func (f *recoveryFixture) request(t *testing.T, action string) delivery.RecoveryRequest {
	t.Helper()
	revision := f.item(t).Revision
	r := delivery.RecoveryRequest{Action: action, Revision: &revision, Actor: "recovery-operator", Reason: "Checked Jira after restart"}
	if action == "reconcile" {
		r.RemoteID = "10001"
	} else {
		r.AcknowledgeDuplicateRisk = true
	}
	return r
}

func TestRecoveryVerifiesAfterCrashAndRestart(t *testing.T) {
	f := newRecoveryFixture(t)
	if d := f.item(t); d.Status != domain.DeliveryUncertain || d.Attempt != 1 {
		t.Fatalf("crash state=%+v", d)
	}
	for _, id := range []string{"absent", "unavailable"} {
		r := f.request(t, "reconcile")
		r.RemoteID = id
		var result delivery.RecoveryResult
		pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", r, 200, &result)
		if result.Delivery.Status != domain.DeliveryUncertain || result.Outcome == "verified" || result.Delivery.Revision != *r.Revision+1 {
			t.Fatalf("non-match resolved delivery: %+v", result)
		}
	}
	f.rebuild(t)
	r := f.request(t, "reconcile")
	pilotHTTP(t, f.server, "POST", f.path, "pilot-webhook", r, 401, nil)
	reads := f.reads.Load()
	bad := r
	bad.Revision = nil
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", bad, 400, nil)
	bad = r
	bad.RemoteID = "../comment"
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", bad, 400, nil)
	if f.reads.Load() != reads {
		t.Fatal("unauthorized or invalid recovery contacted Jira")
	}
	var result delivery.RecoveryResult
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", r, 200, &result)
	if result.Outcome != "verified" || result.Delivery.Status != domain.DeliverySent || result.Delivery.RemoteID != "10001" || result.Delivery.DeliveredAt == nil || f.writes.Load() != 0 {
		t.Fatalf("reconciliation=%+v", result)
	}
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", r, 409, nil)
	if f.reads.Load() != reads+2 {
		t.Fatal("stale decision contacted Jira")
	}
	if _, err := f.store.ClaimReviewedDelivery(context.Background(), "sender", time.Minute); !errors.Is(err, delivery.ErrNoDelivery) {
		t.Fatal("reconciliation queued another send")
	}
	f.rebuild(t)
	d := f.item(t)
	messages, _ := f.store.ListMessages(context.Background(), f.id)
	if d.ReviewedBody != "Operator-edited question?" || d.ReviewedBy != "original-operator" || messages[0].Body != f.draft.Body {
		t.Fatal("recovery rewrote original review or draft")
	}
	events, _ := f.store.Timeline(context.Background(), f.id)
	counts := map[audit.EventType]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	if counts[audit.RequesterSendStarted] != 1 || counts[audit.RequesterDeliveryChecked] != 2 || counts[audit.RequesterDeliveryReconciled] != 1 {
		t.Fatalf("audit counts=%v", counts)
	}
}

func TestConcurrentResendsQueueOnceAndFenceOldSender(t *testing.T) {
	f := newRecoveryFixture(t)
	r := f.request(t, "resend")
	bad := r
	bad.AcknowledgeDuplicateRisk = false
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", bad, 400, nil)
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.recovery.Recover(context.Background(), f.id, f.draft.ID, r)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, delivery.ErrRecoveryConflict) {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || f.reads.Load() != 0 || f.item(t).Status != domain.DeliveryPending {
		t.Fatalf("queued=%d reads=%d", succeeded, f.reads.Load())
	}
	f.rebuild(t)
	claims := make(chan delivery.Job, 12)
	claimErrors := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := f.store.ClaimReviewedDelivery(context.Background(), "new-sender", time.Minute)
			claims <- job
			claimErrors <- err
		}()
	}
	wg.Wait()
	close(claims)
	close(claimErrors)
	claimed := 0
	for err := range claimErrors {
		if err == nil {
			claimed++
		} else if !errors.Is(err, delivery.ErrNoDelivery) {
			t.Fatal(err)
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent claims=%d", claimed)
	}
	var job delivery.Job
	for candidate := range claims {
		if candidate.Delivery.ID != "" {
			job = candidate
		}
	}
	var err error
	if err != nil || job.Delivery.Attempt != 2 || job.Message.Body != "Operator-edited question?" {
		t.Fatalf("recovered job=%+v err=%v", job, err)
	}
	if err := f.store.CompleteDelivery(context.Background(), f.first, "old-response", time.Now()); !errors.Is(err, delivery.ErrLeaseLost) {
		t.Fatal("old sender committed")
	}
	if err := f.store.CompleteDelivery(context.Background(), job, "10002", time.Now()); err != nil {
		t.Fatal(err)
	}
	events, _ := f.store.Timeline(context.Background(), f.id)
	count := 0
	for _, event := range events {
		if event.Type == audit.RequesterResendRequested {
			count++
		}
	}
	if count != 1 || f.item(t).Attempt != 2 {
		t.Fatal("resend history duplicated")
	}
}

func TestRecoverySuppressesSupersededResends(t *testing.T) {
	for _, beforeQueue := range []bool{true, false} {
		t.Run(map[bool]string{true: "reply before resend", false: "reply after resend"}[beforeQueue], func(t *testing.T) {
			f := newRecoveryFixture(t)
			r := f.request(t, "resend")
			if !beforeQueue {
				pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", r, 200, nil)
			}
			pilotHTTP(t, f.server, "POST", "/v1/tickets/jira/replies", "pilot-webhook", jira.ReplyPayload{ExternalID: "RECOVERY-1", CommentID: "reply-1", Author: "requester@example.test", Body: "New information"}, 202, nil)
			if beforeQueue {
				pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", r, 409, nil)
				// Historical delivery can still be verified after a requester reply.
				pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", f.request(t, "reconcile"), 200, nil)
				if f.item(t).Status != domain.DeliverySent {
					t.Fatal("historical verification failed")
				}
			} else {
				if _, err := f.store.ClaimReviewedDelivery(context.Background(), "sender", time.Minute); !errors.Is(err, delivery.ErrNoDelivery) {
					t.Fatal(err)
				}
				if f.item(t).Status != domain.DeliverySuperseded {
					t.Fatal("superseded resend was sent")
				}
			}
		})
	}
}

func TestReconciliationCannotOverwriteConcurrentResend(t *testing.T) {
	f := newRecoveryFixture(t)
	f.blockRead = make(chan struct{})
	f.readStarted = make(chan struct{})
	r := f.request(t, "reconcile")
	done := make(chan error, 1)
	go func() { _, err := f.recovery.Recover(context.Background(), f.id, f.draft.ID, r); done <- err }()
	select {
	case <-f.readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("verification did not start")
	}
	resend := r
	resend.Action = "resend"
	resend.RemoteID = ""
	resend.AcknowledgeDuplicateRisk = true
	_, err := f.recovery.Recover(context.Background(), f.id, f.draft.ID, resend)
	close(f.blockRead)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, delivery.ErrRecoveryConflict) {
		t.Fatalf("stale verification committed: %v", err)
	}
	if f.item(t).Status != domain.DeliveryPending {
		t.Fatal("verification overwrote resend")
	}
}

func TestRecoveryGatesAndAuditRollback(t *testing.T) {
	f := newRecoveryFixture(t)
	r := f.request(t, "resend")
	approval, _ := approvals.NewService(f.store, time.Hour)
	readOnly, _ := api.NewServer(nil, f.store, "", "pilot-operator", api.WithAdmin(f.store, approval, domain.OperatingModeReview), api.WithMessageReviews(f.store, true), api.WithMessageRecovery(f.recovery))
	var payload struct {
		Messages []domain.CaseMessage `json:"messages"`
		Enabled  bool                 `json:"recovery_enabled"`
	}
	pilotHTTP(t, readOnly, "GET", "/v1/investigations/"+string(f.id)+"/messages", "pilot-operator", nil, 200, &payload)
	if len(payload.Messages) != 1 || !payload.Enabled {
		t.Fatal("model-independent recovery reads unavailable")
	}
	shadow, _ := api.NewServer(nil, f.store, "", "pilot-operator", api.WithAdmin(f.store, approval, domain.OperatingModeShadow), api.WithMessageReviews(f.store, true), api.WithMessageRecovery(f.recovery))
	pilotHTTP(t, shadow, "POST", f.path, "pilot-operator", r, 503, nil)
	off, _ := api.NewServer(nil, f.store, "", "pilot-operator", api.WithAdmin(f.store, approval, domain.OperatingModeReview), api.WithMessageReviews(f.store, false), api.WithMessageRecovery(f.recovery))
	pilotHTTP(t, off, "POST", f.path, "pilot-operator", r, 503, nil)
	pilotHTTP(t, f.server, "POST", "/v1/investigations/other/messages/other/recovery", "pilot-operator", r, 404, nil)
	if _, err := f.pool.Exec(context.Background(), `CREATE FUNCTION reject_recovery_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='requester.resend_requested' THEN RAISE EXCEPTION 'test audit failure'; END IF; RETURN NEW; END; $$;
 CREATE TRIGGER fail_recovery_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_recovery_audit()`); err != nil {
		t.Fatal(err)
	}
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", r, 500, nil)
	if d := f.item(t); d.Status != domain.DeliveryUncertain || d.Revision != *r.Revision {
		t.Fatal("failed audit did not roll back recovery")
	}
}

func TestRecoveryOfRejectedDeliveryAndRetryHistory(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", f.request(t, "resend"), 200, nil)
	job, err := f.store.ClaimReviewedDelivery(ctx, "sender", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.FailDelivery(ctx, job, "Jira comment API returned 403 Forbidden", domain.DeliveryFailed, time.Now()); err != nil {
		t.Fatal(err)
	}
	if d := f.item(t); d.Status != domain.DeliveryFailed || d.Attempt != 2 {
		t.Fatalf("rejection=%+v", d)
	}
	pilotHTTP(t, f.server, "POST", f.path, "pilot-operator", f.request(t, "resend"), 200, nil)
	job, err = f.store.ClaimReviewedDelivery(ctx, "sender", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RetryDelivery(ctx, job, "Jira comment API returned 429 Too Many Requests", time.Now(), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	job, err = f.store.ClaimReviewedDelivery(ctx, "sender", time.Minute)
	if err != nil || job.Delivery.Attempt != 4 || job.Message.Body != "Operator-edited question?" {
		t.Fatalf("retry=%+v err=%v", job, err)
	}
	if err := f.store.CompleteDelivery(ctx, job, "10002", time.Now()); err != nil {
		t.Fatal(err)
	}
	events, _ := f.store.Timeline(ctx, f.id)
	counts := map[audit.EventType]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	if counts[audit.RequesterSendStarted] != 4 || counts[audit.RequesterSendRetried] != 1 || counts[audit.RequesterResendRequested] != 2 {
		t.Fatalf("missing durable attempt history: %v", counts)
	}
}
