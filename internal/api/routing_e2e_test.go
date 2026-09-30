package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/models"
	"github.com/wardstone-project/wardstone/internal/policy"
	"github.com/wardstone-project/wardstone/internal/specialists"
	"github.com/wardstone-project/wardstone/internal/worker"
)

// This exercises the complete authenticated intake → leased worker → real
// provider protocol → PostgreSQL lifecycle. Each run owns an isolated schema.
func TestRoutingEndToEnd(t *testing.T) {
	pool := routingTestDatabase(t)
	store := database.NewStore(pool)
	collector := &routingTestCollector{}
	registry := capabilities.NewRegistry()
	if err := registry.Register(
		capabilities.Definition{Name: "test.directory.read", Connector: "test_directory", Description: "Read directory evidence", Effect: domain.EffectRead, ArgumentsVersion: 1},
		capabilities.Definition{Name: "test.access.grant", Connector: "test_directory", Description: "Propose an access grant", Effect: domain.EffectMutate, ArgumentsVersion: 1},
	); err != nil {
		t.Fatal(err)
	}
	profiles, err := specialists.NewRegistry(
		specialists.Profile{Name: specialists.HelpDesk, Instructions: "Ask for missing information and hand off confirmed access issues.", AllowedCollectors: []domain.CapabilityName{"test.directory.read"}, HandoffTargets: []domain.SpecialistName{specialists.AccessManagement}},
		specialists.Profile{Name: specialists.AccessManagement, Instructions: "Investigate access and propose evidence-backed grants.", AllowedCollectors: []domain.CapabilityName{"test.directory.read"}, AllowedCapabilities: []domain.CapabilityName{"test.access.grant"}},
		specialists.Profile{Name: specialists.VendorReview, Instructions: "Review vendor requests with supplied ticket context."},
		specialists.Profile{Name: specialists.SystemsKnowledge, Instructions: "Explain systems using supplied context."},
	)
	if err != nil {
		t.Fatal(err)
	}
	var classifications, diagnoses atomic.Int64
	modelServer := routingModelServer(t, &classifications, &diagnoses)
	t.Cleanup(modelServer.Close)
	model, err := models.NewOpenAICompatible(modelServer.URL, "local-fixture", "routing-fixture", modelServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	intent := specialists.DefaultIntentConfig()
	service, err := investigations.NewService(store, []connectors.EvidenceCollector{collector}, model, registry,
		policy.New(domain.OperatingModeShadow, registry, map[domain.CapabilityName]policy.RuleMode{"test.access.grant": policy.RuleAllow}),
		investigations.Config{MaxConcurrentCollectors: 2, CollectorTimeout: time.Second, ModelTimeout: 2 * time.Second,
			PromptVersion: "routing-e2e-v1", Specialists: profiles, IntentClassification: &intent,
			RoutingRules: []specialists.RouteRule{{Name: "jira-vendor", Specialist: specialists.VendorReview, Classification: "vendor_review", Match: specialists.RouteMatch{Sources: []string{"jira"}, RequestTypes: []string{"Vendor review"}}}},
		})
	if err != nil {
		t.Fatal(err)
	}
	approvalService, err := approvals.NewService(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.NewServer(service, store, "webhook-test-token", "operator-test-token",
		api.WithAdmin(store, approvalService, domain.OperatingModeShadow), api.WithConversations(service), api.WithRoutingPreview(service))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	workers, err := worker.NewPool(store, service, slog.New(slog.NewTextHandler(routingTestLog{t: t}, nil)), 2, 5*time.Second, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		_ = workers.Run(workerCtx)
	}()
	t.Cleanup(func() { stopWorkers(); <-workerDone })

	post := func(path, token string, payload any, expected int) []byte {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, httpServer.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != expected {
			t.Fatalf("%s status = %d, want %d: %s", path, response.StatusCode, expected, data)
		}
		return data
	}
	intake := func(payload map[string]any, expected int, created bool) domain.InvestigationID {
		t.Helper()
		data := post("/v1/tickets/jira", "webhook-test-token", payload, expected)
		var result struct {
			ID      domain.InvestigationID `json:"investigation_id"`
			Created bool                   `json:"created"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.ID == "" || result.Created != created {
			t.Fatalf("unexpected intake response: %s", data)
		}
		return result.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Operator preview must neither invoke a provider nor persist anything.
	vendor := map[string]any{"external_id": "E2E-VENDOR", "summary": "Review vendor", "request_type": "Vendor review"}
	for _, token := range []string{"", "webhook-test-token"} {
		post("/v1/admin/routing/preview", token, vendor, http.StatusUnauthorized)
	}
	post("/v1/tickets/jira", "operator-test-token", vendor, http.StatusUnauthorized)
	preview := post("/v1/admin/routing/preview", "operator-test-token", vendor, http.StatusOK)
	var previewResult struct {
		Routing domain.RoutingDecision `json:"routing"`
	}
	if err := json.Unmarshal(preview, &previewResult); err != nil {
		t.Fatal(err)
	}
	if previewResult.Routing.Source != domain.RoutingSourceRule || previewResult.Routing.RuleName != "jira-vendor" {
		t.Fatalf("preview decision = %+v", previewResult.Routing)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tickets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preview persisted tickets: count=%d err=%v", count, err)
	}
	if classifications.Load() != 0 || diagnoses.Load() != 0 || collector.calls.Load() != 0 {
		t.Fatal("preview invoked a model or collector")
	}

	vendorID := intake(vendor, http.StatusAccepted, true)
	vendorResult := awaitRoutingInvestigation(t, ctx, store, vendorID, domain.InvestigationCompleted)
	if vendorResult.Specialist != specialists.VendorReview || vendorResult.Routing == nil || vendorResult.Routing.Source != domain.RoutingSourceRule {
		t.Fatalf("structured route = %+v", vendorResult)
	}
	if classifications.Load() != 0 || collector.calls.Load() != 0 {
		t.Fatal("structured vendor route invoked classifier or unauthorized collector")
	}
	if duplicateID := intake(vendor, http.StatusOK, false); duplicateID != vendorID {
		t.Fatal("duplicate ticket created a new investigation")
	}

	ambiguousVendor := map[string]any{"external_id": "E2E-MODEL-VENDOR", "summary": "Assess an unfamiliar supplier"}
	modelID := intake(ambiguousVendor, http.StatusAccepted, true)
	modelResult := awaitRoutingInvestigation(t, ctx, store, modelID, domain.InvestigationCompleted)
	if modelResult.Specialist != specialists.VendorReview || modelResult.Routing == nil || modelResult.Routing.Source != domain.RoutingSourceModel || modelResult.Routing.Confidence == nil || *modelResult.Routing.Confidence != .9 {
		t.Fatalf("model-selected vendor route = %+v", modelResult)
	}
	if classifications.Load() != 1 || collector.calls.Load() != 0 {
		t.Fatal("model-selected vendor route widened collector permissions")
	}

	ambiguousHelp := map[string]any{"external_id": "E2E-HELP", "summary": "Something is broken", "reporter_email": "requester@example.test"}
	helpID := intake(ambiguousHelp, http.StatusAccepted, true)
	helpResult := awaitRoutingInvestigation(t, ctx, store, helpID, domain.InvestigationWaiting)
	if helpResult.Specialist != specialists.HelpDesk || helpResult.Routing == nil || helpResult.Routing.Source != domain.RoutingSourceFallback || helpResult.Routing.FallbackCode != "low_confidence" {
		t.Fatalf("low-confidence fallback = %+v", helpResult)
	}
	originalRoute := *helpResult.Routing
	messages, err := store.ListMessages(ctx, helpID)
	if err != nil || len(messages) != 1 || messages[0].Direction != domain.MessageOutbound || messages[0].Body != "Which system needs access?" {
		t.Fatalf("durable question = %+v, err=%v", messages, err)
	}
	reply := map[string]any{"external_id": "E2E-HELP", "comment_id": "E2E-REPLY-1", "author": "requester@example.test", "body": "I need access to Engineering."}
	post("/v1/tickets/jira/replies", "operator-test-token", reply, http.StatusUnauthorized)
	wrongAuthor := map[string]any{"external_id": "E2E-HELP", "comment_id": "E2E-REPLY-UNAUTHORIZED", "author": "other@example.test", "body": "Grant me access."}
	post("/v1/tickets/jira/replies", "webhook-test-token", wrongAuthor, http.StatusForbidden)
	post("/v1/tickets/jira/replies", "webhook-test-token", reply, http.StatusAccepted)
	completed := awaitRoutingInvestigation(t, ctx, store, helpID, domain.InvestigationCompleted)
	if completed.Specialist != specialists.AccessManagement || completed.Routing == nil || !completed.Routing.Equal(originalRoute) {
		t.Fatalf("handoff did not retain initial routing: %+v", completed)
	}
	post("/v1/tickets/jira/replies", "webhook-test-token", reply, http.StatusOK)
	// Finish all leased jobs before checking call counts, so duplicate requests
	// cannot appear idempotent merely because another worker has not run yet.
	awaitRoutingJobs(t, ctx, pool, 5)
	if classifications.Load() != 2 || diagnoses.Load() != 5 || collector.calls.Load() != 3 {
		t.Fatalf("unexpected lifecycle calls: classifier=%d diagnosis=%d collectors=%d", classifications.Load(), diagnoses.Load(), collector.calls.Load())
	}
	messages, err = store.ListMessages(ctx, helpID)
	if err != nil {
		t.Fatal(err)
	}
	inbound := 0
	for _, message := range messages {
		if message.Direction == domain.MessageInbound {
			inbound++
		}
	}
	if inbound != 1 {
		t.Fatalf("duplicate reply persisted %d inbound messages", inbound)
	}
	timeline, err := store.Timeline(ctx, helpID)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[audit.EventType]int)
	for _, event := range timeline {
		counts[event.Type]++
	}
	for _, event := range []audit.EventType{audit.DispatcherRouted, audit.RequesterQuestionAsked, audit.RequesterReplyReceived, audit.SpecialistHandedOff, audit.ActionProposed} {
		if counts[event] != 1 {
			t.Errorf("audit %s count = %d, want 1", event, counts[event])
		}
	}
	var decision domain.PolicyDecision
	var reason string
	if err := pool.QueryRow(ctx, `SELECT policy_decision, policy_reason FROM proposed_actions WHERE investigation_id = $1`, helpID).Scan(&decision, &reason); err != nil || decision != domain.PolicyDeny || reason != "shadow_mode" {
		t.Fatalf("proposal ignored SHADOW policy: decision=%s reason=%s err=%v", decision, reason, err)
	}
}

func routingTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("WARDSTONE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WARDSTONE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	control, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal("connect integration database")
	}
	schema := "routing_e2e_" + string(domain.NewTicketID())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := control.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		_ = control.Close(ctx)
		t.Fatal("create isolated integration schema")
	}
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// The identifier comes exclusively from the generated schema above.
		if _, err := control.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error("remove isolated integration schema")
		}
		_ = control.Close(cleanupCtx)
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse integration database configuration")
	}
	config.ConnConfig.RuntimeParams["search_path"] = identifier
	pool, err = pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("open isolated integration pool")
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v", err)
	}
	sort.Strings(files)
	for _, file := range files {
		migration, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
	return pool
}

func awaitRoutingInvestigation(t *testing.T, ctx context.Context, store *database.Store, id domain.InvestigationID, status domain.InvestigationStatus) domain.Investigation {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		item, err := store.GetInvestigation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if item.Status == status {
			return item
		}
		if item.Status == domain.InvestigationFailed {
			t.Fatalf("investigation failed: %s", item.Failure)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("investigation %s did not reach %s (last status %s)", id, status, item.Status)
		case <-ticker.C:
		}
	}
}

func awaitRoutingJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected int) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var total, completed, dead int
		if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'COMPLETED'), count(*) FILTER (WHERE status = 'DEAD') FROM jobs`).Scan(&total, &completed, &dead); err != nil {
			t.Fatal(err)
		}
		if total != expected || dead != 0 {
			t.Fatalf("unexpected durable jobs: total=%d completed=%d dead=%d, want %d completed", total, completed, dead, expected)
		}
		if completed == expected {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker jobs did not complete before the deadline")
		case <-ticker.C:
		}
	}
}

type routingTestCollector struct{ calls atomic.Int64 }

type routingTestLog struct{ t *testing.T }

func (log routingTestLog) Write(data []byte) (int, error) {
	log.t.Log(string(data))
	return len(data), nil
}

func (*routingTestCollector) Name() domain.ConnectorName        { return "test_directory" }
func (*routingTestCollector) Capability() domain.CapabilityName { return "test.directory.read" }
func (c *routingTestCollector) Collect(context.Context, domain.Ticket) ([]domain.Evidence, error) {
	c.calls.Add(1)
	return []domain.Evidence{{Source: "test_directory", Kind: "directory_state", Summary: "Requested group membership is missing", Data: json.RawMessage(`{"member":false}`)}}, nil
}

func routingModelServer(t *testing.T, classifications, diagnoses *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var completion struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if request.URL.Path != "/chat/completions" || json.NewDecoder(request.Body).Decode(&completion) != nil || len(completion.Messages) != 2 {
			http.Error(writer, "invalid fixture request", http.StatusBadRequest)
			return
		}
		var input map[string]json.RawMessage
		if json.Unmarshal([]byte(completion.Messages[1].Content), &input) != nil {
			http.Error(writer, "invalid model input", http.StatusBadRequest)
			return
		}
		var output any
		if _, classify := input["candidates"]; classify {
			classifications.Add(1)
			var intent agent.IntentRequest
			if json.Unmarshal([]byte(completion.Messages[1].Content), &intent) != nil || len(intent.Candidates) != 4 || len(input) != 2 || intent.Ticket.ID != "" || intent.Ticket.ExternalID != "" || intent.Ticket.ReporterEmail != "" {
				http.Error(writer, "classifier received unexpected context", http.StatusBadRequest)
				return
			}
			confidence := .9
			if intent.Ticket.Summary == "Something is broken" {
				confidence = .2
			}
			output = agent.IntentResult{Specialist: specialists.VendorReview, Classification: "vendor_review", Confidence: confidence, Reason: "Supplier review intent."}
		} else {
			diagnoses.Add(1)
			var diagnosis agent.Request
			if json.Unmarshal([]byte(completion.Messages[1].Content), &diagnosis) != nil {
				http.Error(writer, "invalid diagnosis request", http.StatusBadRequest)
				return
			}
			switch diagnosis.Specialist {
			case specialists.VendorReview:
				if len(diagnosis.Evidence) != 0 {
					http.Error(writer, "vendor received unauthorized evidence", http.StatusBadRequest)
					return
				}
				output = agent.Result{Diagnosis: "Vendor review completed with ticket context."}
			case specialists.HelpDesk:
				if len(diagnosis.Conversation) == 0 {
					output = agent.Result{FollowUpQuestion: "Which system needs access?"}
				} else {
					output = agent.Result{Handoff: &agent.Handoff{Specialist: specialists.AccessManagement, Reason: "Requester confirmed an access issue."}}
				}
			case specialists.AccessManagement:
				if len(diagnosis.Evidence) == 0 {
					http.Error(writer, "access evidence missing", http.StatusBadRequest)
					return
				}
				output = agent.Result{Diagnosis: "Missing membership confirmed; grant proposed for operator review.", Actions: []agent.ProposedAction{{Capability: "test.access.grant", Arguments: json.RawMessage(`{"group":"Engineering"}`), Reason: "Requester and directory evidence confirm missing access.", EvidenceIDs: []domain.EvidenceID{diagnosis.Evidence[0].ID}}}}
			default:
				http.Error(writer, "unexpected role", http.StatusBadRequest)
				return
			}
		}
		content, err := json.Marshal(output)
		if err != nil {
			http.Error(writer, "fixture encoding failed", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}}}})
	}))
}
