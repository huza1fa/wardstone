package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/connectors/google"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/models"
	"github.com/wardstone-project/wardstone/internal/policy"
	"github.com/wardstone-project/wardstone/internal/specialists"
	"github.com/wardstone-project/wardstone/internal/worker"
)

const accessRequester = "requester@example.test"
const accessGroup = "engineering@example.test"
const accessQuestion = "Which configured resource do you need? Reply with exactly Engineering."

// These tests use the full migration chain in an isolated PostgreSQL schema.
// Only the remote providers are HTTP fixtures: intake, conversation, collection,
// OpenAI decoding, investigation validation, policy, and persistence are real.
func TestAccessHelpEndToEndRequesterResumeAfterRestart(t *testing.T) {
	f := newAccessHelpFixture(t, accessHelpScenario{})
	ticket := f.ticket("ACCESS-RESTART", "")
	f.request(http.MethodPost, "/v1/tickets/jira", "operator-test-token", ticket, http.StatusUnauthorized, nil)
	id := f.intake(ticket, true)
	f.runNext(id, "")
	waiting := f.detail(id, domain.InvestigationWaiting)
	if waiting.WorkPackage != nil || waiting.Diagnosis != "" || waiting.Routing == nil || waiting.Routing.Source != domain.RoutingSourceRule {
		t.Fatalf("unexpected waiting case: %+v", waiting)
	}
	messages := f.messages(id)
	if len(messages) != 1 || messages[0].Direction != domain.MessageOutbound || messages[0].Body != accessQuestion {
		t.Fatalf("question not persisted: %+v", messages)
	}
	initial := f.requests()[0]
	observation, initialEvidence := accessObservation(t, initial)
	if observation.Status != "clarification_required" || observation.UserEmail != accessRequester || !accessHasUnknown(observation, "resource", "missing_resource") {
		t.Fatalf("missing resource observation = %+v", observation)
	}
	f.assertGoogleReads(0)
	f.assertJobs(1)
	f.assertNoSideEffects(id)

	// Drop every application object and the database connections before replying.
	// The replacement service gets conversation, route, and evidence only from PG.
	f.restart()
	if got := f.messages(id); !reflect.DeepEqual(got, messages) {
		t.Fatalf("restart lost durable question: got %+v, want %+v", got, messages)
	}
	if duplicate := f.intake(ticket, false); duplicate != id {
		t.Fatal("duplicate ticket changed investigation ID")
	}
	reply := jira.ReplyPayload{ExternalID: ticket.ExternalID, CommentID: "reply-1", Author: accessRequester, Body: "Engineering"}
	f.request(http.MethodPost, "/v1/tickets/jira/replies", "", reply, http.StatusUnauthorized, nil)
	f.request(http.MethodPost, "/v1/tickets/jira/replies", "operator-test-token", reply, http.StatusUnauthorized, nil)
	forbidden := reply
	forbidden.CommentID, forbidden.Author = "forbidden-reply", "other@example.test"
	f.request(http.MethodPost, "/v1/tickets/jira/replies", "webhook-test-token", forbidden, http.StatusForbidden, nil)
	f.assertJobs(1)
	if len(f.messages(id)) != 1 {
		t.Fatal("forbidden reply was persisted")
	}
	var received accessHelpIntakeResult
	f.request(http.MethodPost, "/v1/tickets/jira/replies", "webhook-test-token", reply, http.StatusAccepted, &received)
	if received.ID != id || !received.Created {
		t.Fatalf("reply created a different case: %+v", received)
	}
	// Retry both before and after execution, proving no second resume job exists.
	f.request(http.MethodPost, "/v1/tickets/jira/replies", "webhook-test-token", reply, http.StatusOK, &received)
	if received.ID != id || received.Created {
		t.Fatalf("reply retry was not idempotent: %+v", received)
	}
	f.runNext(id, "")
	completed := f.detail(id, domain.InvestigationCompleted)
	if completed.Specialist != specialists.AccessManagement || !completed.Routing.Equal(*waiting.Routing) {
		t.Fatalf("resume changed initial routing: %+v", completed)
	}
	requests := f.requests()
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(requests))
	}
	resumed := requests[1]
	if len(resumed.Conversation) != 2 || resumed.Conversation[0].Body != accessQuestion || resumed.Conversation[1].Body != "Engineering" || resumed.Conversation[1].Author != accessRequester {
		t.Fatalf("replacement service did not reload conversation: %+v", resumed.Conversation)
	}
	observation, currentEvidence := accessObservation(t, resumed)
	if observation.ResourceSource != "requester_message" || observation.Resource != "Engineering" || observation.GroupEmail != accessGroup || observation.DirectMembership != "not_member" || observation.User == nil || observation.User.Suspended {
		t.Fatalf("resumed access observation = %+v", observation)
	}
	if currentEvidence.ID == initialEvidence.ID || len(resumed.Evidence) != 2 {
		t.Fatalf("resume did not refresh and retain evidence: %+v", resumed.Evidence)
	}
	if resumed.Evidence[1].ID != initialEvidence.ID || !strings.Contains(strings.Join(resumed.Warnings, " "), string(initialEvidence.ID)) {
		t.Fatal("durable prior observation was not marked historical")
	}
	f.assertReport(id, completed, resumed)
	f.assertProposal(id, accessGroup, accessRequester, currentEvidence.ID)
	messages = f.messages(id)
	if len(messages) != 3 || messages[1].Direction != domain.MessageInbound || messages[1].ExternalID != reply.CommentID || messages[2].Direction != domain.MessageOutbound || !strings.Contains(messages[2].Body, completed.WorkPackage.RequesterSummary) || !strings.Contains(messages[2].Body, "DENY") {
		t.Fatalf("unexpected durable conversation: %+v", messages)
	}
	f.request(http.MethodPost, "/v1/tickets/jira/replies", "webhook-test-token", reply, http.StatusOK, &received)
	if received.ID != id || received.Created || f.intake(ticket, false) != id {
		t.Fatal("completed-case retry was not idempotent")
	}
	f.assertJobs(2)
	f.assertGoogleReads(3)
	if len(f.requests()) != 2 || len(f.messages(id)) != 3 {
		t.Fatal("duplicate requests caused additional work or messages")
	}
	f.assertEvents(id, map[audit.EventType]int{
		audit.TicketReceived: 1, audit.DispatcherRouted: 1, audit.RequesterQuestionAsked: 1,
		audit.RequesterReplyReceived: 1, audit.InvestigationResumed: 1, audit.EvidenceCollected: 2,
		audit.ActionProposed: 1, audit.PolicyEvaluated: 1, audit.InvestigationCompleted: 1,
	})
	f.assertNoSideEffects(id)
}

func TestAccessHelpEndToEndNonGrantFixtures(t *testing.T) {
	for _, test := range []struct {
		name, resource, membership, status string
		scenario                           accessHelpScenario
		reads                              int
		unknown                            google.AccessUnknown
	}{
		{name: "already member", resource: "Engineering", membership: "member", status: "observed", scenario: accessHelpScenario{member: true}, reads: 3},
		{name: "suspended user", resource: "Engineering", membership: "not_member", status: "observed", scenario: accessHelpScenario{suspended: true}, reads: 3},
		{name: "membership forbidden", resource: "Engineering", membership: "unknown", status: "incomplete_observation", scenario: accessHelpScenario{membershipForbidden: true}, reads: 3, unknown: google.AccessUnknown{Field: "direct_membership", Reason: "forbidden"}},
		{name: "unknown resource", resource: "Unconfigured", membership: "unknown", status: "operator_intervention_required", reads: 0, unknown: google.AccessUnknown{Field: "resource", Reason: "unknown_resource"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAccessHelpFixture(t, test.scenario)
			id := f.intake(f.ticket("ACCESS-NON-GRANT", test.resource), true)
			f.runNext(id, "")
			completed := f.detail(id, domain.InvestigationCompleted)
			requests := f.requests()
			if len(requests) != 1 {
				t.Fatalf("model calls = %d, want 1", len(requests))
			}
			observation, _ := accessObservation(t, requests[0])
			if observation.Status != test.status || observation.DirectMembership != test.membership || (test.unknown.Field != "" && !accessHasUnknown(observation, test.unknown.Field, test.unknown.Reason)) {
				t.Fatalf("incorrect observation: %+v", observation)
			}
			if test.scenario.suspended && (observation.User == nil || !observation.User.Suspended) {
				t.Fatal("suspended state did not reach model")
			}
			f.assertReport(id, completed, requests[0])
			if completed.WorkPackage.EscalationReason == "" || len(completed.WorkPackage.Unknowns) == 0 {
				t.Fatalf("non-grant report lost uncertainty/escalation: %+v", completed.WorkPackage)
			}
			f.assertCount(`SELECT count(*) FROM proposed_actions`, 0)
			f.assertEvents(id, map[audit.EventType]int{audit.ActionProposed: 0, audit.PolicyEvaluated: 0, audit.InvestigationCompleted: 1})
			f.assertJobs(1)
			f.assertGoogleReads(test.reads)
			f.assertNoSideEffects(id)
		})
	}
}

func TestAccessHelpEndToEndInvalidCitationsFailAtomically(t *testing.T) {
	for _, invalid := range []string{"report", "action"} {
		t.Run(invalid, func(t *testing.T) {
			f := newAccessHelpFixture(t, accessHelpScenario{invalidCitation: invalid})
			id := f.intake(f.ticket("ACCESS-INVALID-CITATION", "Engineering"), true)
			f.runNext(id, "unknown evidence")
			failed := f.detail(id, domain.InvestigationFailed)
			if failed.WorkPackage != nil || failed.Diagnosis != "" || !strings.Contains(failed.Failure, "unknown evidence") {
				t.Fatalf("invalid citation did not fail cleanly: %+v", failed)
			}
			f.assertCount(`SELECT count(*) FROM investigations WHERE work_package IS NOT NULL`, 0)
			f.assertCount(`SELECT count(*) FROM proposed_actions`, 0)
			f.assertCount(`SELECT count(*) FROM evidence`, 1)
			if len(f.messages(id)) != 0 {
				t.Fatal("failed diagnosis persisted a requester result")
			}
			f.assertEvents(id, map[audit.EventType]int{audit.InvestigationFailed: 1, audit.InvestigationCompleted: 0, audit.DiagnosisGenerated: 0, audit.ActionProposed: 0})
			f.assertGoogleReads(3)
			f.assertNoSideEffects(id)
		})
	}
}

// Fresh citations alone cannot justify a change: the installed access workflow
// deterministically binds the proposal to a complete, matching inspection.
func TestAccessHelpEndToEndRejectsContradictoryProposals(t *testing.T) {
	for _, test := range []struct {
		name, resource string
		scenario       accessHelpScenario
	}{
		{name: "suspended user", resource: "Engineering", scenario: accessHelpScenario{suspended: true}},
		{name: "already member", resource: "Engineering", scenario: accessHelpScenario{member: true}},
		{name: "unknown membership", resource: "Engineering", scenario: accessHelpScenario{membershipForbidden: true}},
		{name: "unknown resource", resource: "Unconfigured"},
		{name: "wrong group", resource: "Engineering", scenario: accessHelpScenario{proposalGroup: "admins@example.test"}},
		{name: "wrong user", resource: "Engineering", scenario: accessHelpScenario{proposalUser: "other@example.test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.scenario.forceProposal = true
			f := newAccessHelpFixture(t, test.scenario)
			id := f.intake(f.ticket("ACCESS-SEMANTIC-GAP", test.resource), true)
			f.runNext(id, "proposal validation failed")
			failed := f.detail(id, domain.InvestigationFailed)
			if failed.WorkPackage != nil || failed.Diagnosis != "" {
				t.Fatalf("contradictory proposal persisted a result: %+v", failed)
			}
			f.assertCount(`SELECT count(*) FROM proposed_actions`, 0)
			if len(f.messages(id)) != 0 {
				t.Fatal("contradictory proposal persisted a requester draft")
			}
			f.assertEvents(id, map[audit.EventType]int{audit.InvestigationFailed: 1, audit.InvestigationCompleted: 0, audit.ActionProposed: 0, audit.PolicyEvaluated: 0})
			f.assertNoSideEffects(id)
		})
	}
}

type accessHelpScenario struct {
	member, suspended, membershipForbidden, forceProposal bool
	invalidCitation, proposalGroup, proposalUser          string
}

type accessHelpIntakeResult struct {
	ID      domain.InvestigationID `json:"investigation_id"`
	Created bool                   `json:"created"`
}

type accessHelpFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	store    *database.Store
	service  *investigations.Service
	api      *httptest.Server
	google   *httptest.Server
	model    *httptest.Server
	scenario accessHelpScenario
	mu       sync.Mutex
	inputs   []agent.Request
	reads    map[string]int
	writes   int
}

func newAccessHelpFixture(t *testing.T, scenario accessHelpScenario) *accessHelpFixture {
	t.Helper()
	// The shared helper gates on WARDSTONE_TEST_DATABASE_URL, runs every migration,
	// and removes only this test's generated schema after all clients are closed.
	pool := routingTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	f := &accessHelpFixture{t: t, ctx: ctx, pool: pool, scenario: scenario, reads: make(map[string]int)}
	f.google = httptest.NewServer(http.HandlerFunc(f.serveGoogle))
	f.model = httptest.NewServer(http.HandlerFunc(f.serveModel))
	t.Cleanup(f.google.Close)
	t.Cleanup(f.model.Close)
	f.openApplication()
	return f
}

func (f *accessHelpFixture) openApplication() {
	f.t.Helper()
	f.store = database.NewStore(f.pool)
	client, err := google.NewHTTPClient(f.google.URL, "google-fixture-token", f.google.Client())
	if err != nil {
		f.t.Fatal(err)
	}
	collector, err := google.NewAccessCollector(client, map[string]string{"Engineering": accessGroup})
	if err != nil {
		f.t.Fatal(err)
	}
	model, err := models.NewOpenAICompatible(f.model.URL, "model-fixture-token", "access-fixture", f.model.Client())
	if err != nil {
		f.t.Fatal(err)
	}
	registry := capabilities.NewRegistry()
	if err := registry.Register(google.Capabilities()...); err != nil {
		f.t.Fatal(err)
	}
	profiles, err := specialists.NewRegistry(
		specialists.Profile{Name: specialists.HelpDesk, Instructions: "Clarify requests."},
		specialists.Profile{Name: specialists.AccessManagement, Instructions: "Inspect configured access and direct membership. Propose grants only for active nonmembers with known target identities. Escalate unknowns; never claim execution.",
			AllowedCollectors: []domain.CapabilityName{google.AccessInspect}, AllowedCapabilities: []domain.CapabilityName{google.GroupsAddMember}},
	)
	if err != nil {
		f.t.Fatal(err)
	}
	f.service, err = investigations.NewService(f.store, []connectors.EvidenceCollector{collector}, model, registry,
		policy.New(domain.OperatingModeShadow, registry, map[domain.CapabilityName]policy.RuleMode{google.GroupsAddMember: policy.RuleAllow}),
		investigations.Config{MaxConcurrentCollectors: 1, CollectorTimeout: 2 * time.Second, ModelTimeout: 2 * time.Second, PromptVersion: "access-e2e-v1",
			Specialists: profiles, DeliverRequesterMessages: false,
			RoutingRules: []specialists.RouteRule{{Name: "jira-access", Specialist: specialists.AccessManagement, Classification: "access_management", Match: specialists.RouteMatch{Sources: []string{"jira"}, RequestTypes: []string{"Access help"}}}},
		})
	if err != nil {
		f.t.Fatal(err)
	}
	server, err := api.NewServer(f.service, f.store, "webhook-test-token", "operator-test-token", api.WithConversations(f.service))
	if err != nil {
		f.t.Fatal(err)
	}
	f.api = httptest.NewServer(server.Handler())
	f.t.Cleanup(f.api.Close)
}

func (f *accessHelpFixture) restart() {
	f.t.Helper()
	f.api.Close()
	config := f.pool.Config()
	f.pool.Close()
	pool, err := pgxpool.NewWithConfig(f.ctx, config)
	if err != nil {
		f.t.Fatal(err)
	}
	f.pool = pool
	f.t.Cleanup(pool.Close)
	f.openApplication()
}

func (f *accessHelpFixture) ticket(externalID, resource string) jira.TicketPayload {
	ticket := jira.TicketPayload{ExternalID: externalID, Summary: "Help with Engineering access", ReporterEmail: accessRequester, RequestType: "Access help"}
	if resource != "" {
		ticket.Fields = map[string][]string{"Access resource": {resource}}
	}
	return ticket
}

func (f *accessHelpFixture) request(method, path, token string, payload any, status int, result any) {
	f.t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			f.t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(f.ctx, method, f.api.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.api.Client().Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		f.t.Fatal(err)
	}
	if response.StatusCode != status {
		f.t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, status, data)
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *accessHelpFixture) intake(ticket jira.TicketPayload, created bool) domain.InvestigationID {
	f.t.Helper()
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	var result accessHelpIntakeResult
	f.request(http.MethodPost, "/v1/tickets/jira", "webhook-test-token", ticket, status, &result)
	if result.ID == "" || result.Created != created {
		f.t.Fatalf("unexpected intake: %+v", result)
	}
	return result.ID
}

// Execute one real leased queue job synchronously, making the restart boundary
// deterministic and avoiding background retries masking citation failures.
func (f *accessHelpFixture) runNext(id domain.InvestigationID, failure string) {
	f.t.Helper()
	job, err := f.store.Claim(f.ctx, "access-e2e-worker", 10*time.Second)
	if err != nil || job.InvestigationID != id {
		f.t.Fatalf("claim next job: %+v, err=%v", job, err)
	}
	err = f.service.Run(worker.WithLease(f.ctx, job), id)
	if failure != "" {
		if err == nil || !strings.Contains(err.Error(), failure) {
			f.t.Fatalf("run error = %v, want %q", err, failure)
		}
		if err := f.store.Fail(f.ctx, job, err.Error()); err != nil {
			f.t.Fatal(err)
		}
		return
	}
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Complete(f.ctx, job); err != nil {
		f.t.Fatal(err)
	}
}

func (f *accessHelpFixture) detail(id domain.InvestigationID, status domain.InvestigationStatus) domain.Investigation {
	f.t.Helper()
	path := "/v1/investigations/" + string(id)
	f.request(http.MethodGet, path, "webhook-test-token", nil, http.StatusUnauthorized, nil)
	var item domain.Investigation
	f.request(http.MethodGet, path, "operator-test-token", nil, http.StatusOK, &item)
	if item.ID != id || item.Status != status {
		f.t.Fatalf("detail = %+v, want %s", item, status)
	}
	return item
}

func (f *accessHelpFixture) messages(id domain.InvestigationID) []domain.CaseMessage {
	f.t.Helper()
	var result struct {
		Messages []domain.CaseMessage `json:"messages"`
	}
	f.request(http.MethodGet, "/v1/investigations/"+string(id)+"/messages", "operator-test-token", nil, http.StatusOK, &result)
	return result.Messages
}

func (f *accessHelpFixture) assertReport(id domain.InvestigationID, item domain.Investigation, request agent.Request) {
	f.t.Helper()
	if item.WorkPackage == nil || item.WorkPackage.Version != 1 || item.WorkPackage.Summary != item.Diagnosis || len(item.WorkPackage.Findings) != 1 || item.WorkPackage.Findings[0].Kind != "verified" {
		f.t.Fatalf("missing/invalid work package: %+v", item.WorkPackage)
	}
	if err := item.WorkPackage.Validate(request.Evidence); err != nil {
		f.t.Fatal(err)
	}
	_, current := accessObservation(f.t, request)
	if !reflect.DeepEqual(item.WorkPackage.Findings[0].EvidenceIDs, []domain.EvidenceID{current.ID}) {
		f.t.Fatal("report did not cite current evidence")
	}
	// Read JSONB through an independent Store, rather than accepting an in-memory result.
	persisted, err := database.NewStore(f.pool).GetInvestigation(f.ctx, id)
	if err != nil || !reflect.DeepEqual(item.WorkPackage, persisted.WorkPackage) {
		f.t.Fatalf("work package persistence mismatch: %+v, err=%v", persisted.WorkPackage, err)
	}
}

func (f *accessHelpFixture) assertProposal(id domain.InvestigationID, group, user string, evidenceID domain.EvidenceID) {
	f.t.Helper()
	f.assertCount(`SELECT count(*) FROM proposed_actions`, 1)
	var capability domain.CapabilityName
	var arguments, citations []byte
	var decision domain.PolicyDecision
	var reason, digest string
	err := f.pool.QueryRow(f.ctx, `SELECT capability, arguments, evidence_ids, policy_decision, policy_reason, action_digest FROM proposed_actions WHERE investigation_id = $1`, id).
		Scan(&capability, &arguments, &citations, &decision, &reason, &digest)
	if err != nil {
		f.t.Fatal(err)
	}
	var args map[string]string
	var ids []domain.EvidenceID
	if json.Unmarshal(arguments, &args) != nil || json.Unmarshal(citations, &ids) != nil || capability != google.GroupsAddMember || !reflect.DeepEqual(args, map[string]string{"group": group, "user": user}) || !reflect.DeepEqual(ids, []domain.EvidenceID{evidenceID}) || digest == "" || decision != domain.PolicyDeny || reason != "shadow_mode" {
		f.t.Fatalf("incorrect persisted proposal: capability=%s arguments=%s citations=%s policy=%s/%s digest=%s", capability, arguments, citations, decision, reason, digest)
	}
}

func (f *accessHelpFixture) assertEvents(id domain.InvestigationID, expected map[audit.EventType]int) {
	f.t.Helper()
	path := "/v1/investigations/" + string(id) + "/timeline"
	f.request(http.MethodGet, path, "webhook-test-token", nil, http.StatusUnauthorized, nil)
	var result struct {
		Events []audit.Event `json:"events"`
	}
	f.request(http.MethodGet, path, "operator-test-token", nil, http.StatusOK, &result)
	counts := make(map[audit.EventType]int)
	for i, event := range result.Events {
		if event.InvestigationID != id || event.Sequence != int64(i+1) {
			f.t.Fatalf("invalid durable timeline ordering: %+v", event)
		}
		counts[event.Type]++
	}
	for event, want := range expected {
		if counts[event] != want {
			f.t.Errorf("audit %s count = %d, want %d", event, counts[event], want)
		}
	}
}

func (f *accessHelpFixture) assertCount(query string, expected int) {
	f.t.Helper()
	var count int
	if err := f.pool.QueryRow(f.ctx, query).Scan(&count); err != nil || count != expected {
		f.t.Fatalf("%s: count=%d, want %d, err=%v", query, count, expected, err)
	}
}

func (f *accessHelpFixture) assertJobs(expected int) {
	f.t.Helper()
	f.assertCount(`SELECT count(*) FROM jobs`, expected)
	f.assertCount(`SELECT count(*) FROM jobs WHERE status = 'COMPLETED' AND attempt = 1`, expected)
	if _, err := f.store.Claim(f.ctx, "duplicate-check", time.Second); !errors.Is(err, worker.ErrNoJob) {
		f.t.Fatalf("unexpected extra claimable job: %v", err)
	}
}

func (f *accessHelpFixture) assertNoSideEffects(id domain.InvestigationID) {
	f.t.Helper()
	f.mu.Lock()
	writes := f.writes
	f.mu.Unlock()
	if writes != 0 {
		f.t.Fatalf("Google write requests = %d", writes)
	}
	f.assertCount(`SELECT count(*) FROM executions`, 0)
	f.assertCount(`SELECT count(*) FROM approvals`, 0)
	f.assertCount(`SELECT count(*) FROM message_deliveries`, 0)
	// No Jira send can occur: even a real delivery dispatcher has nothing to claim.
	if _, err := f.store.ClaimDelivery(f.ctx, "jira-send-check", time.Second); !errors.Is(err, delivery.ErrNoDelivery) {
		f.t.Fatalf("unexpected Jira delivery work: %v", err)
	}
	f.assertEvents(id, map[audit.EventType]int{audit.ActionExecuted: 0, audit.TicketUpdated: 0, audit.RequesterMessageDelivered: 0})
}

func (f *accessHelpFixture) assertGoogleReads(expected int) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, count := range f.reads {
		total += count
	}
	if total != expected {
		f.t.Fatalf("Google reads = %+v, want %d total", f.reads, expected)
	}
	if expected == 3 && !reflect.DeepEqual(f.reads, map[string]int{"/users/" + accessRequester: 1, "/groups/" + accessGroup: 1, "/groups/group-1/members/user-1": 1}) {
		f.t.Fatalf("unexpected Google selectors or retries: %+v", f.reads)
	}
}

func (f *accessHelpFixture) requests() []agent.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Request(nil), f.inputs...)
}

func (f *accessHelpFixture) serveGoogle(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	if request.Method == http.MethodGet {
		f.reads[request.URL.Path]++
	} else {
		f.writes++
	}
	f.mu.Unlock()
	if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer google-fixture-token" || request.URL.RawQuery != "" {
		f.t.Error("unexpected Google method, authorization, or query")
		http.Error(writer, "invalid Google fixture request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	switch request.URL.Path {
	case "/users/" + accessRequester:
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "user-1", "primaryEmail": accessRequester, "suspended": f.scenario.suspended})
	case "/groups/" + accessGroup:
		_ = json.NewEncoder(writer).Encode(google.Group{ID: "group-1", Email: accessGroup, Name: "Engineering"})
	case "/groups/group-1/members/user-1":
		if f.scenario.membershipForbidden {
			http.Error(writer, "Directory membership read forbidden", http.StatusForbidden)
		} else if !f.scenario.member {
			http.Error(writer, "Direct member not found", http.StatusNotFound)
		} else {
			_ = json.NewEncoder(writer).Encode(google.Member{ID: "user-1", Email: accessRequester, Type: "USER", Role: "MEMBER"})
		}
	default:
		f.t.Errorf("unexpected Google endpoint: %s", request.URL.Path)
		http.NotFound(writer, request)
	}
}

func (f *accessHelpFixture) serveModel(writer http.ResponseWriter, request *http.Request) {
	var completion struct {
		Model    string `json:"model"`
		Messages []struct {
			Role, Content string
		} `json:"messages"`
	}
	if request.Method != http.MethodPost || request.URL.Path != "/chat/completions" || request.Header.Get("Authorization") != "Bearer model-fixture-token" || json.NewDecoder(request.Body).Decode(&completion) != nil || completion.Model != "access-fixture" || len(completion.Messages) != 2 || completion.Messages[1].Role != "user" {
		f.t.Error("invalid OpenAI fixture request")
		http.Error(writer, "invalid model request", http.StatusBadRequest)
		return
	}
	var input agent.Request
	if json.Unmarshal([]byte(completion.Messages[1].Content), &input) != nil || input.Specialist != specialists.AccessManagement || len(input.Evidence) == 0 {
		f.t.Error("missing access model context")
		http.Error(writer, "invalid model context", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.inputs = append(f.inputs, input)
	f.mu.Unlock()
	// Current evidence is first; historical observations must not drive grants.
	var observation google.AccessEvidenceV1
	if json.Unmarshal(input.Evidence[0].Data, &observation) != nil || input.Evidence[0].Capability != google.AccessInspect || input.Evidence[0].ID == "" {
		f.t.Error("invalid real collector observation")
		http.Error(writer, "invalid observation", http.StatusBadRequest)
		return
	}
	var output agent.Result
	if accessHasUnknown(observation, "resource", "missing_resource") && !f.scenario.forceProposal {
		output.FollowUpQuestion = accessQuestion
	} else {
		statement := "Direct membership inspection is " + observation.DirectMembership + "."
		output.WorkPackage = &domain.WorkPackage{Version: 1, Summary: "Configured access inspection completed for operator review.",
			Findings:         []domain.Finding{{Kind: "verified", Statement: statement, EvidenceIDs: []domain.EvidenceID{input.Evidence[0].ID}}},
			Unknowns:         []string{"Effective application access and authorization were not assessed."},
			RequesterSummary: "Your configured access was inspected. An operator must review the next steps.",
			ExpectedImpact:   "No access has been changed by this investigation.",
		}
		eligible := observation.Status == "observed" && observation.User != nil && !observation.User.Suspended && observation.Group != nil && observation.DirectMembership == "not_member"
		if eligible || f.scenario.forceProposal {
			group, user := observation.GroupEmail, observation.UserEmail
			if f.scenario.forceProposal {
				group, user = accessGroup, accessRequester
			}
			if f.scenario.proposalGroup != "" {
				group = f.scenario.proposalGroup
			}
			if f.scenario.proposalUser != "" {
				user = f.scenario.proposalUser
			}
			args, _ := json.Marshal(map[string]string{"group": group, "user": user})
			output.Actions = []agent.ProposedAction{{Capability: google.GroupsAddMember, Arguments: args, Reason: "Propose direct membership for operator review; no authorization or execution claimed.", EvidenceIDs: []domain.EvidenceID{input.Evidence[0].ID}}}
		} else {
			output.WorkPackage.EscalationReason = "A grant is not indicated by the bounded observations; operator investigation is required."
		}
		if f.scenario.invalidCitation == "report" {
			output.WorkPackage.Findings[0].EvidenceIDs = []domain.EvidenceID{"evd_not_supplied"}
		} else if f.scenario.invalidCitation == "action" {
			output.Actions[0].EvidenceIDs = []domain.EvidenceID{"evd_not_supplied"}
		}
	}
	content, err := json.Marshal(output)
	if err != nil {
		f.t.Error(err)
		http.Error(writer, "fixture encoding failed", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}}}})
}

func accessObservation(t *testing.T, request agent.Request) (google.AccessEvidenceV1, domain.Evidence) {
	t.Helper()
	if len(request.Evidence) == 0 {
		t.Fatal("model did not receive access evidence")
	}
	evidence := request.Evidence[0]
	var observation google.AccessEvidenceV1
	if err := json.Unmarshal(evidence.Data, &observation); err != nil || evidence.ID == "" || evidence.InvestigationID != request.InvestigationID || evidence.Source != google.Name || evidence.Capability != google.AccessInspect || evidence.Kind != "google.access_request" || observation.Version != 1 {
		t.Fatalf("invalid access evidence: %+v, err=%v", evidence, err)
	}
	return observation, evidence
}

func accessHasUnknown(observation google.AccessEvidenceV1, field, reason string) bool {
	for _, unknown := range observation.Unknowns {
		if unknown.Field == field && unknown.Reason == reason {
			return true
		}
	}
	return false
}
