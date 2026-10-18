package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/connectors/research"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/policy"
	"github.com/wardstone-project/wardstone/internal/specialists"
	"github.com/wardstone-project/wardstone/internal/worker"
)

type pilotModel struct {
	requests []agent.Request
	respond  func(agent.Request) agent.Result
}

func (*pilotModel) Name() domain.ModelProviderName { return "pilot-fixture" }
func (*pilotModel) Model() string                  { return "test-model" }
func (m *pilotModel) Diagnose(_ context.Context, r agent.Request) (agent.Result, error) {
	m.requests = append(m.requests, r)
	return m.respond(r), nil
}

func pilotService(t *testing.T, pool *pgxpool.Pool, model *pilotModel, collectors []connectors.EvidenceCollector, profiles []specialists.Profile, rules []specialists.RouteRule, riskConfig ...domain.VendorRiskConfig) (*database.Store, *investigations.Service, *api.Server) {
	t.Helper()
	store := database.NewStore(pool)
	registry := capabilities.NewRegistry()
	if err := registry.Register(research.Capabilities()...); err != nil {
		t.Fatal(err)
	}
	roles, err := specialists.NewRegistry(profiles...)
	if err != nil {
		t.Fatal(err)
	}
	intent := specialists.DefaultIntentConfig()
	intent.Enabled = false
	vendorRisk := domain.VendorRiskConfig{}
	if len(riskConfig) > 0 {
		vendorRisk = riskConfig[0]
	}
	service, err := investigations.NewService(store, collectors, model, registry, policy.New(domain.OperatingModeReview, registry, nil), investigations.Config{VendorRisk: vendorRisk, MaxConcurrentCollectors: 2, CollectorTimeout: time.Second, ModelTimeout: time.Second, PromptVersion: "pilot-test", Specialists: roles, RoutingRules: rules, IntentClassification: &intent})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := approvals.NewService(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.NewServer(service, store, "pilot-webhook", "pilot-operator", api.WithAdmin(store, approval, domain.OperatingModeReview), api.WithConversations(service), api.WithMessageReviews(store, true))
	if err != nil {
		t.Fatal(err)
	}
	return store, service, server
}

func pilotHTTP(t *testing.T, s *api.Server, method, path, token string, payload any, status int, result any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, w.Code, status, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatal(err)
		}
	}
}

func pilotRun(t *testing.T, store *database.Store, service *investigations.Service, id domain.InvestigationID) {
	t.Helper()
	ctx := context.Background()
	job, err := store.Claim(ctx, "pilot-worker", time.Minute)
	if err != nil || job.InvestigationID != id {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if err := service.Run(worker.WithLease(ctx, job), id); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, job); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedJiraConversationEndToEnd(t *testing.T) {
	pool := routingTestDatabase(t)
	ctx := context.Background()
	model := &pilotModel{respond: func(r agent.Request) agent.Result {
		if len(r.Conversation) == 0 {
			return agent.Result{FollowUpQuestion: "Which system needs help?"}
		}
		return agent.Result{Diagnosis: "Ready for operator review", WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Ready for operator review", Unknowns: []string{"No connector configured"}, EscalationReason: "Operator must investigate", RequesterSummary: "Your reply was received"}}
	}}
	profiles := []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Clarify missing context and escalate unavailable evidence"}}
	store, service, server := pilotService(t, pool, model, nil, profiles, nil)
	var intake struct {
		ID domain.InvestigationID `json:"investigation_id"`
	}
	ticket := jira.TicketPayload{ExternalID: "REVIEW-1", Summary: "Something is broken", ReporterEmail: "requester@example.test"}
	pilotHTTP(t, server, "POST", "/v1/tickets/jira", "pilot-webhook", ticket, 202, &intake)
	pilotRun(t, store, service, intake.ID)
	messages, err := store.ListMessages(ctx, intake.ID)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	draft := messages[0]
	if items, err := store.ListDeliveries(ctx, intake.ID); err != nil || len(items) != 0 {
		t.Fatal("model draft queued without review")
	}
	path := "/v1/investigations/" + string(intake.ID) + "/messages/" + string(draft.ID) + "/review"
	review := delivery.Review{Body: "Which application and exact error do you see?", Actor: "operator@example.test", DraftDigest: delivery.DraftDigest(draft.Body)}
	pilotHTTP(t, server, "POST", path, "pilot-webhook", review, 401, nil)
	bad := review
	bad.DraftDigest = delivery.DraftDigest("wrong draft")
	pilotHTTP(t, server, "POST", path, "pilot-operator", bad, 409, nil)
	bad.Actor = ""
	pilotHTTP(t, server, "POST", path, "pilot-operator", bad, 400, nil)
	approval, _ := approvals.NewService(store, time.Hour)
	shadow, _ := api.NewServer(nil, store, "", "pilot-operator", api.WithAdmin(store, approval, domain.OperatingModeShadow), api.WithMessageReviews(store, true))
	pilotHTTP(t, shadow, "POST", path, "pilot-operator", review, 503, nil)
	// Competing reviews serialize on the case; exactly one enqueues and audits.
	results := make(chan bool, 12)
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := store.ReviewMessage(ctx, intake.ID, draft.ID, review, time.Now().UTC())
			results <- created
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for created := range results {
		if created {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("created deliveries=%d", count)
	}
	pilotHTTP(t, server, "POST", path, "pilot-operator", review, 200, nil)
	changed := review
	changed.Body = "Changed approval"
	pilotHTTP(t, server, "POST", path, "pilot-operator", changed, 409, nil)
	job, err := store.ClaimReviewedDelivery(ctx, "pilot-sender", time.Minute)
	if err != nil || job.Message.Body != review.Body {
		t.Fatalf("approved send=%+v err=%v", job, err)
	}
	if err := store.CompleteDelivery(ctx, job, "jira-comment-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimReviewedDelivery(ctx, "pilot-sender", time.Minute); !errors.Is(err, delivery.ErrNoDelivery) {
		t.Fatalf("duplicate send claim=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE message_deliveries SET reviewed_body='tampered' WHERE id=$1`, job.Delivery.ID); err == nil {
		t.Fatal("review body was mutable")
	}
	// Reconstruct the entire application service before receiving the reply.
	resumedModel := &pilotModel{respond: model.respond}
	store, service, server = pilotService(t, pool, resumedModel, nil, profiles, nil)
	reply := jira.ReplyPayload{ExternalID: ticket.ExternalID, CommentID: "reply-1", Author: ticket.ReporterEmail, Body: "VPN authentication expired"}
	pilotHTTP(t, server, "POST", "/v1/tickets/jira/replies", "pilot-webhook", reply, 202, nil)
	pilotHTTP(t, server, "POST", "/v1/tickets/jira/replies", "pilot-webhook", reply, 200, nil)
	pilotRun(t, store, service, intake.ID)
	if len(resumedModel.requests) != 1 || resumedModel.requests[0].Conversation[0].Body != review.Body {
		t.Fatal("resumed model did not see operator-edited question")
	}
	ledger, _ := store.ListMessages(ctx, intake.ID)
	if ledger[0].Body != draft.Body {
		t.Fatal("original draft was overwritten")
	}
	events, _ := store.Timeline(ctx, intake.ID)
	counts := map[audit.EventType]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	if counts[audit.RequesterMessageReviewed] != 1 || counts[audit.RequesterMessageDelivered] != 1 || counts[audit.RequesterReplyReceived] != 1 {
		t.Fatalf("duplicate audit events: %+v", counts)
	}
	var payload struct {
		Enabled    bool                     `json:"review_enabled"`
		Deliveries []domain.MessageDelivery `json:"deliveries"`
		Digests    map[string]string        `json:"draft_digests"`
	}
	pilotHTTP(t, server, "GET", "/v1/investigations/"+string(intake.ID)+"/messages", "pilot-operator", nil, 200, &payload)
	if !payload.Enabled || len(payload.Deliveries) != 1 || payload.Deliveries[0].Status != domain.DeliverySent || payload.Digests[string(draft.ID)] != review.DraftDigest {
		t.Fatalf("review read model=%+v", payload)
	}
}

func TestReviewedDeliveryCrashRequiresReconciliation(t *testing.T) {
	pool := routingTestDatabase(t)
	model := &pilotModel{respond: func(agent.Request) agent.Result { return agent.Result{FollowUpQuestion: "Which system?"} }}
	store, service, server := pilotService(t, pool, model, nil, []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Clarify"}}, nil)
	var intake struct {
		ID domain.InvestigationID `json:"investigation_id"`
	}
	pilotHTTP(t, server, "POST", "/v1/tickets/jira", "pilot-webhook", jira.TicketPayload{ExternalID: "CRASH-1", Summary: "Help"}, 202, &intake)
	pilotRun(t, store, service, intake.ID)
	messages, _ := store.ListMessages(context.Background(), intake.ID)
	draft := messages[0]
	if _, _, err := store.ReviewMessage(context.Background(), intake.ID, draft.ID, delivery.Review{Body: draft.Body, Actor: "operator", DraftDigest: delivery.DraftDigest(draft.Body)}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimReviewedDelivery(context.Background(), "crashed-sender", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE message_deliveries SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, job.Delivery.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimReviewedDelivery(context.Background(), "new-sender", time.Minute); !errors.Is(err, delivery.ErrNoDelivery) {
		t.Fatalf("unknown outcome reclaimed for another POST: %v", err)
	}
	items, _ := store.ListDeliveries(context.Background(), intake.ID)
	if len(items) != 1 || items[0].Status != domain.DeliveryUncertain || items[0].LastError != "delivery_outcome_unknown" {
		t.Fatalf("missing reconciliation state: %+v", items)
	}
}

func TestResearchSpecialistsEndToEnd(t *testing.T) {
	pool := routingTestDatabase(t)
	reads := 0
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("The vendor claims encrypted transport. Its public document does not establish customer configuration."))
	}))
	defer source.Close()
	vendor, err := research.NewCollector(research.VendorCollect, research.Catalog{"Tailscale": {{Title: "Vendor security", URL: source.URL}}}, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	knowledge, err := research.NewCollector(research.KnowledgeCollect, research.Catalog{"VPN": {{Title: "VPN runbook", URL: source.URL}}}, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	model := &pilotModel{respond: func(r agent.Request) agent.Result {
		var doc *domain.Evidence
		for i := range r.Evidence {
			if r.Evidence[i].Kind == "research.vendor_document" || r.Evidence[i].Kind == "research.knowledge_document" {
				doc = &r.Evidence[i]
			}
		}
		if doc == nil {
			return agent.Result{FollowUpQuestion: "Reply with exactly Tailscale"}
		}
		report := &domain.WorkPackage{Version: 1, Summary: "Executive summary based on configured sources", Findings: []domain.Finding{{Kind: "verified", Statement: "The collected document states encrypted transport", EvidenceIDs: []domain.EvidenceID{doc.ID}}}, RequesterSummary: "Source-backed result", Unknowns: []string{"Actual deployment configuration"}}
		if r.Specialist == specialists.VendorReview {
			return agent.Result{Diagnosis: report.Summary, WorkPackage: report, VendorAssessment: &domain.VendorAssessment{Criteria: []domain.VendorCriterionAssessment{}}}
		}
		return agent.Result{Diagnosis: report.Summary, WorkPackage: report}
	}}
	profiles := []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Help"}, {Name: specialists.VendorReview, Instructions: "Review vendor", AllowedCollectors: []domain.CapabilityName{research.VendorCollect}}, {Name: specialists.SystemsKnowledge, Instructions: "Read systems runbook", AllowedCollectors: []domain.CapabilityName{research.KnowledgeCollect}}}
	rules := []specialists.RouteRule{{Name: "vendor", Specialist: specialists.VendorReview, Classification: "vendor_review", Match: specialists.RouteMatch{RequestTypes: []string{"Vendor review"}}}, {Name: "knowledge", Specialist: specialists.SystemsKnowledge, Classification: "systems_knowledge", Match: specialists.RouteMatch{RequestTypes: []string{"Systems question"}}}}
	store, service, server := pilotService(t, pool, model, []connectors.EvidenceCollector{vendor, knowledge}, profiles, rules)
	for _, tc := range []struct {
		key, request, field, value string
		role                       domain.SpecialistName
	}{{"VENDOR-1", "Vendor review", "Vendor", "Tailscale", specialists.VendorReview}, {"KNOWLEDGE-1", "Systems question", "Knowledge topic", "VPN", specialists.SystemsKnowledge}} {
		var intake struct {
			ID domain.InvestigationID `json:"investigation_id"`
		}
		pilotHTTP(t, server, "POST", "/v1/tickets/jira", "pilot-webhook", jira.TicketPayload{ExternalID: tc.key, Summary: "Investigate", RequestType: tc.request, Fields: map[string][]string{tc.field: {tc.value}}}, 202, &intake)
		pilotRun(t, store, service, intake.ID)
		item, err := store.GetInvestigation(context.Background(), intake.ID)
		if err != nil || item.Status != domain.InvestigationCompleted || item.Specialist != tc.role || len(item.WorkPackage.Findings) != 1 {
			t.Fatalf("case=%+v err=%v", item, err)
		}
		if tc.role == specialists.VendorReview && item.WorkPackage.RiskAssessment.Rating != "UNKNOWN" {
			t.Fatal("risk assessment lost")
		}
		events, _ := store.Timeline(context.Background(), intake.ID)
		tools := 0
		for _, e := range events {
			if e.Type == audit.ToolInvoked {
				tools++
			}
		}
		if tools != 1 {
			t.Fatalf("specialist collected beyond its role: tools=%d", tools)
		}
		if d, _ := store.ListDeliveries(context.Background(), intake.ID); len(d) != 0 {
			t.Fatal("specialist sent an unreviewed message")
		}
	}
	if reads != 2 {
		t.Fatalf("unexpected source reads=%d", reads)
	}
}

func TestSupersededReviewsAndPendingSendsAreRejected(t *testing.T) {
	pool := routingTestDatabase(t)
	model := &pilotModel{respond: func(r agent.Request) agent.Result {
		if len(r.Conversation) == 0 {
			return agent.Result{FollowUpQuestion: "Which system?"}
		}
		return agent.Result{Diagnosis: "Ready for operator review", WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Ready for operator review"}}
	}}
	store, service, server := pilotService(t, pool, model, nil, []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Clarify missing context"}}, nil)
	for _, reviewBeforeReply := range []bool{false, true} {
		key := "SUPERSEDED-UNREVIEWED"
		if reviewBeforeReply {
			key = "SUPERSEDED-QUEUED"
		}
		var intake struct {
			ID domain.InvestigationID `json:"investigation_id"`
		}
		pilotHTTP(t, server, "POST", "/v1/tickets/jira", "pilot-webhook", jira.TicketPayload{ExternalID: key, Summary: "Help", ReporterEmail: "requester@example.test"}, 202, &intake)
		pilotRun(t, store, service, intake.ID)
		messages, _ := store.ListMessages(context.Background(), intake.ID)
		draft := messages[0]
		review := delivery.Review{Body: draft.Body, Actor: "operator", DraftDigest: delivery.DraftDigest(draft.Body)}
		path := "/v1/investigations/" + string(intake.ID) + "/messages/" + string(draft.ID) + "/review"
		if reviewBeforeReply {
			pilotHTTP(t, server, "POST", path, "pilot-operator", review, 202, nil)
		}
		pilotHTTP(t, server, "POST", "/v1/tickets/jira/replies", "pilot-webhook", jira.ReplyPayload{ExternalID: key, CommentID: key + "-reply", Author: "requester@example.test", Body: "VPN"}, 202, nil)
		if reviewBeforeReply {
			if _, err := store.ClaimReviewedDelivery(context.Background(), "sender", time.Minute); !errors.Is(err, delivery.ErrNoDelivery) {
				t.Fatalf("superseded question was sent: %v", err)
			}
			deliveries, _ := store.ListDeliveries(context.Background(), intake.ID)
			if len(deliveries) != 1 || deliveries[0].Status != domain.DeliverySuperseded || deliveries[0].LastError != "draft_superseded" {
				t.Fatalf("superseded delivery=%+v", deliveries)
			}
		} else {
			pilotHTTP(t, server, "POST", path, "pilot-operator", review, 409, nil)
		}
		messages, _ = store.ListMessages(context.Background(), intake.ID)
		inbound := messages[len(messages)-1]
		inboundPath := "/v1/investigations/" + string(intake.ID) + "/messages/" + string(inbound.ID) + "/review"
		review.DraftDigest = delivery.DraftDigest(inbound.Body)
		pilotHTTP(t, server, "POST", inboundPath, "pilot-operator", review, 409, nil)
		pilotRun(t, store, service, intake.ID)
	}
}
