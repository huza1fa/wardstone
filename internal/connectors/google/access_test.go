package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func accessTicket() domain.Ticket {
	return domain.Ticket{Source: "jira", ReporterEmail: "requester@example.com", Metadata: domain.TicketMetadata{Fields: map[string][]string{"Access resource": {"Engineering tools"}}}}
}

func decodeAccess(t *testing.T, evidence []domain.Evidence, err error) AccessEvidenceV1 {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].Kind != "google.access_request" {
		t.Fatalf("evidence = %+v", evidence)
	}
	var result AccessEvidenceV1
	if err := json.Unmarshal(evidence[0].Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Escalations == nil {
		t.Fatalf("escalation_requirements must be a canonical array: %s", evidence[0].Data)
	}
	return result
}

func TestIncompleteAccessRequestsNeverCallGoogle(t *testing.T) {
	t.Parallel()
	for name, fields := range map[string]map[string][]string{
		"missing resource":                   {},
		"ambiguous resource":                 {"Access resource": {"Engineering tools", "Other"}},
		"unknown resource":                   {"Access resource": {"unconfigured"}},
		"resource URL":                       {"Access resource": {"https://example.com/?group=x"}},
		"resource whitespace":                {"Access resource": {" Engineering tools "}},
		"blank resource":                     {"Access resource": {""}},
		"duplicate field":                    {"Access resource": {"Engineering tools"}, " access RESOURCE ": {"Engineering tools"}},
		"empty explicit user":                {"Access resource": {"Engineering tools"}, "Access user": {}},
		"blank explicit user":                {"Access resource": {"Engineering tools"}, "Access user": {""}},
		"ambiguous user":                     {"Access resource": {"Engineering tools"}, "Access user": {"a@example.com", "b@example.com"}},
		"display name":                       {"Access resource": {"Engineering tools"}, "Access user": {"Person <a@example.com>"}},
		"unsafe query":                       {"Access resource": {"Engineering tools"}, "Access user": {"user@example.com?admin=true"}},
		"missing resource and explicit user": {"Access user": {""}},
		"duplicate user field":               {"Access resource": {"Engineering tools"}, "Access user": {"target@example.com"}, " access USER ": {"target@example.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			client, _ := NewHTTPClient(server.URL, "secret", server.Client())
			collector, _ := NewAccessCollector(client, map[string]string{"Engineering tools": "tools@example.com"})
			ticket := accessTicket()
			ticket.Metadata.Fields = fields
			evidence, err := collector.Collect(context.Background(), ticket)
			result := decodeAccess(t, evidence, err)
			wantStatus := "operator_intervention_required"
			if name == "missing resource" {
				wantStatus = "clarification_required"
			}
			if calls.Load() != 0 || result.Status != wantStatus || len(result.Unknowns) == 0 || result.DirectMembership != "unknown" {
				t.Fatalf("calls=%d result=%+v", calls.Load(), result)
			}
			_, resourcePresent := fields["Access resource"]
			if resourcePresent && len(result.Clarifications) != 0 || !resourcePresent && len(result.Clarifications) != 1 {
				t.Fatalf("only absent resource should request clarification: %+v", result)
			}
			if wantStatus == "operator_intervention_required" {
				if len(result.Escalations) == 0 {
					t.Fatalf("missing operator correction: %+v", result)
				}
				for _, escalation := range result.Escalations {
					if !strings.Contains(escalation, "Operator must") || !strings.Contains(escalation, "new case") || !strings.Contains(escalation, "deduplicates") || !strings.Contains(escalation, "does not refresh") {
						t.Fatalf("incorrect immutable-selector remediation: %q", escalation)
					}
				}
				if !resourcePresent && !strings.Contains(result.Clarifications[0], "replying alone cannot resolve") {
					t.Fatalf("mixed error implies reply is sufficient: %+v", result)
				}
			} else if len(result.Escalations) != 0 {
				t.Fatalf("reply-resolvable resource escalated: %+v", result)
			}
			if values := fields["Access resource"]; name != "duplicate field" && len(values) == 1 && values[0] == "Engineering tools" && result.Resource != "Engineering tools" {
				t.Fatalf("lost known resource: %+v", result)
			}
		})
	}
}

func TestAccessRequesterReplyResolution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body     string
		direction      domain.MessageDirection
		author, source string
		later          string
		wantCalls      int32
	}{
		{"exact", "  engineering TOOLS  ", domain.MessageInbound, "requester@example.com", "jira", "", 3},
		{"keyword", "Please give Engineering tools", domain.MessageInbound, "requester@example.com", "jira", "", 0},
		{"model outbound", "Engineering tools", domain.MessageOutbound, "requester@example.com", "jira", "", 0},
		{"different requester", "Engineering tools", domain.MessageInbound, "other@example.com", "jira", "", 0},
		{"different source", "Engineering tools", domain.MessageInbound, "requester@example.com", "model", "", 0},
		{"latest only", "Engineering tools", domain.MessageInbound, "requester@example.com", "jira", "I do not know", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := directoryFixture(t, &calls, "", "", "", 0, "requester@example.com")
			defer server.Close()
			client, _ := NewHTTPClient(server.URL, "secret", server.Client())
			collector, _ := NewAccessCollector(client, map[string]string{"Engineering tools": "tools@example.com"})
			ticket := accessTicket()
			ticket.Metadata.Fields = nil
			messages := []domain.CaseMessage{{Source: domain.ConnectorName(tc.source), ExternalID: "comment1", Direction: tc.direction, Author: tc.author, Body: tc.body, CreatedAt: time.Unix(1, 0)}}
			if tc.later != "" {
				messages = append(messages, domain.CaseMessage{Source: "jira", ExternalID: "comment2", Direction: domain.MessageInbound, Author: ticket.ReporterEmail, Body: tc.later, CreatedAt: time.Unix(2, 0)})
			}
			evidence, err := collector.CollectCase(context.Background(), ticket, messages)
			result := decodeAccess(t, evidence, err)
			if calls.Load() != tc.wantCalls {
				t.Fatalf("calls=%d result=%+v", calls.Load(), result)
			}
			if tc.wantCalls == 3 && (result.ResourceSource != "requester_message" || result.Resource != "Engineering tools" || result.DirectMembership != "member" || result.UserSource != "reporter" || result.UserEmail != ticket.ReporterEmail) {
				t.Fatalf("result=%+v", result)
			}
			if len(result.Escalations) != 0 || (tc.wantCalls == 0 && (result.Status != "clarification_required" || len(result.Clarifications) != 1)) {
				t.Fatalf("absent-resource reply behavior: %+v", result)
			}
		})
	}
}

func TestRequesterReplyCannotOverrideExplicitMalformedOrUnknownFields(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	client, _ := NewHTTPClient(server.URL, "secret", server.Client())
	collector, _ := NewAccessCollector(client, map[string]string{"Engineering tools": "tools@example.com"})
	for _, values := range [][]string{nil, {"unknown"}, {""}, {"Engineering tools", "Other"}} {
		ticket := accessTicket()
		ticket.Metadata.Fields["Access resource"] = values
		messages := []domain.CaseMessage{{Source: "jira", ExternalID: "comment1", Direction: domain.MessageInbound, Author: ticket.ReporterEmail, Body: "Engineering tools", CreatedAt: time.Now()}}
		evidence, err := collector.CollectCase(context.Background(), ticket, messages)
		result := decodeAccess(t, evidence, err)
		if result.Status != "operator_intervention_required" || result.Resource != "" || calls.Load() != 0 || len(result.Escalations) != 1 || len(result.Clarifications) != 0 {
			t.Fatalf("reply overrode explicit field: %+v", result)
		}
	}
	ticket := accessTicket()
	ticket.ReporterEmail = ""
	evidence, err := collector.Collect(context.Background(), ticket)
	result := decodeAccess(t, evidence, err)
	if result.Resource != "Engineering tools" || result.UserEmail != "" || result.Status != "operator_intervention_required" || calls.Load() != 0 || len(result.Escalations) != 1 || len(result.Clarifications) != 0 {
		t.Fatalf("missing reporter = %+v", result)
	}
}

func TestInvalidTargetRequiresNewCaseEvenWithExactResourceReply(t *testing.T) {
	t.Parallel()
	collector, _ := NewAccessCollector(&HTTPClient{}, map[string]string{"Engineering tools": "tools@example.com"})
	for _, tc := range []struct {
		name, reporter string
		fields         map[string][]string
	}{
		{"missing reporter", "", nil},
		{"malformed reporter", "Requester <requester@example.com>", nil},
		{"missing explicit target", "requester@example.com", map[string][]string{"Access user": nil}},
		{"malformed explicit target", "requester@example.com", map[string][]string{"Access user": {"target@example.com?unsafe=true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticket := accessTicket()
			ticket.ReporterEmail = tc.reporter
			ticket.Metadata.Fields = tc.fields
			message := domain.CaseMessage{Source: "jira", ExternalID: "comment1", Direction: domain.MessageInbound, Author: "requester@example.com", Body: "Engineering tools", CreatedAt: time.Now()}
			evidence, err := collector.CollectCase(context.Background(), ticket, []domain.CaseMessage{message})
			result := decodeAccess(t, evidence, err)
			if result.Status != "operator_intervention_required" || len(result.Escalations) != 1 || result.DirectMembership != "unknown" || result.User != nil || result.Group != nil {
				t.Fatalf("target repair loop: %+v", result)
			}
			if !strings.Contains(result.Escalations[0], "new case") {
				t.Fatalf("missing new case requirement: %+v", result)
			}
			if tc.fields == nil {
				if len(result.Clarifications) != 1 || !strings.Contains(result.Clarifications[0], "replying alone cannot resolve") {
					t.Fatalf("missing resource overrides escalation: %+v", result)
				}
			} else if result.Resource != "Engineering tools" || len(result.Clarifications) != 0 {
				t.Fatalf("resolved resource lost: %+v", result)
			}
		})
	}
}

func TestAccessEvidenceCanonicalEmptyEscalations(t *testing.T) {
	t.Parallel()
	evidence, err := accessEvidence(AccessEvidenceV1{})
	result := decodeAccess(t, evidence, err)
	if len(result.Escalations) != 0 || !strings.Contains(string(evidence[0].Data), `"escalation_requirements":[]`) {
		t.Fatalf("empty escalations schema: %s", evidence[0].Data)
	}
}

func directoryFixture(t *testing.T, calls *atomic.Int32, userBody, groupBody, memberBody string, status int, email string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		var body string
		switch r.URL.Path {
		case "/users/" + email:
			body = userBody
			if body == "" {
				body = `{"id":"user123","primaryEmail":"` + email + `","suspended":false}`
			}
		case "/groups/tools@example.com":
			body = groupBody
			if body == "" {
				body = `{"id":"group123","email":"tools@example.com","name":"Tools"}`
			}
		case "/groups/group123/members/user123":
			body = memberBody
			if body == "" {
				body = `{"id":"user123","email":"` + email + `","type":"USER","role":"MEMBER"}`
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		if body == "ERROR" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("secret provider body"))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestAccessDirectoryObservations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, user, group, member   string
		status                      int
		wantMembership, wantUnknown string
		wantCalls                   int32
	}{
		{"already member", "", "", "", 0, "member", "", 3},
		{"suspended", `{"id":"user123","primaryEmail":"target@example.com","suspended":true}`, "", "", 0, "member", "", 3},
		{"missing member", "", "", "ERROR", 404, "not_member", "", 3},
		{"member forbidden", "", "", "ERROR", 403, "unknown", "forbidden", 3},
		{"member unavailable", "", "", "ERROR", 500, "unknown", "unavailable", 3},
		{"user missing", "ERROR", "", "", 404, "unknown", "not_found", 2},
		{"group missing", "", "ERROR", "", 404, "unknown", "not_found", 2},
		{"user forbidden", "ERROR", "", "", 403, "unknown", "forbidden", 2},
		{"group forbidden", "", "ERROR", "", 403, "unknown", "forbidden", 2},
		{"wrong user", `{"id":"user123","primaryEmail":"wrong@example.com","suspended":false}`, "", "", 0, "unknown", "invalid_response", 2},
		{"missing suspended", `{"id":"user123","primaryEmail":"target@example.com"}`, "", "", 0, "unknown", "invalid_response", 2},
		{"duplicate identity", `{"id":"wrong123","id":"user123","primaryEmail":"target@example.com","suspended":false}`, "", "", 0, "unknown", "invalid_response", 2},
		{"malformed suspended", `{"id":"user123","primaryEmail":"target@example.com","suspended":"false"}`, "", "", 0, "unknown", "invalid_response", 2},
		{"missing user id", `{"primaryEmail":"target@example.com","suspended":false}`, "", "", 0, "unknown", "invalid_response", 2},
		{"wrong group", "", `{"id":"group123","email":"other@example.com"}`, "", 0, "unknown", "invalid_response", 2},
		{"unsafe group id", "", `{"id":"../groups?admin=true","email":"tools@example.com"}`, "", 0, "unknown", "invalid_response", 2},
		{"wrong member id", "", "", `{"id":"wrong123","email":"target@example.com","type":"USER","role":"MEMBER"}`, 0, "unknown", "invalid_response", 3},
		{"wrong member email", "", "", `{"id":"user123","email":"wrong@example.com","type":"USER","role":"MEMBER"}`, 0, "unknown", "invalid_response", 3},
		{"group member type", "", "", `{"id":"user123","email":"target@example.com","type":"GROUP","role":"MEMBER"}`, 0, "unknown", "invalid_response", 3},
		{"invalid role", "", "", `{"id":"user123","email":"target@example.com","type":"USER","role":"ADMIN"}`, 0, "unknown", "invalid_response", 3},
		{"manager", "", "", `{"id":"user123","email":"target@example.com","type":"USER","role":"MANAGER"}`, 0, "member", "", 3},
		{"owner", "", "", `{"id":"user123","email":"target@example.com","type":"USER","role":"OWNER"}`, 0, "member", "", 3},
		{"oversized", strings.Repeat(" ", 64<<10) + `{}`, "", "", 0, "unknown", "invalid_response", 2},
		{"trailing JSON", `{} {}`, "", "", 0, "unknown", "invalid_response", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := directoryFixture(t, &calls, tc.user, tc.group, tc.member, tc.status, "target@example.com")
			defer server.Close()
			client, _ := NewHTTPClient(server.URL, "secret", server.Client())
			collector, _ := NewAccessCollector(client, map[string]string{"Engineering tools": "tools@example.com"})
			ticket := accessTicket()
			ticket.Metadata.Fields["Access user"] = []string{"target@example.com"}
			evidence, err := collector.Collect(context.Background(), ticket)
			result := decodeAccess(t, evidence, err)
			if result.DirectMembership != tc.wantMembership || calls.Load() != tc.wantCalls || result.UserEmail != "target@example.com" || result.UserSource != "ticket_field" {
				t.Fatalf("calls=%d result=%+v", calls.Load(), result)
			}
			if tc.wantUnknown != "" && (len(result.Unknowns) == 0 || result.Unknowns[0].Reason != tc.wantUnknown) {
				t.Fatalf("unknowns=%+v", result.Unknowns)
			}
			if tc.name == "suspended" && (result.User == nil || !result.User.Suspended) {
				t.Fatalf("missing suspended observation: %+v", result)
			}
			if strings.Contains(string(evidence[0].Data), "secret provider") {
				t.Fatal("provider error leaked")
			}
		})
	}
}

func TestAccessCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	client, _ := NewHTTPClient(server.URL, "secret", server.Client())
	collector, _ := NewAccessCollector(client, map[string]string{"Engineering tools": "tools@example.com"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := collector.Collect(ctx, accessTicket()); done <- err }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("independent reads did not start in parallel")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation ignored")
	}
}

func TestAccessHTTPDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client, _ := NewHTTPClient(server.URL, "secret", server.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.ReadAccessUser(ctx, "requester@example.com")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

type failingAccessTransport struct{}

func (failingAccessTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("secret token in endpoint and transport error")
}

func TestAccessHTTPTransportFailureIsSanitized(t *testing.T) {
	t.Parallel()
	client, err := NewHTTPClient("https://directory.example.com", "secret", &http.Client{Transport: failingAccessTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ReadAccessUser(context.Background(), "requester@example.com")
	failure, ok := connectors.ProbeFailureOf(err)
	if !ok || failure != connectors.ProbeUnavailable || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsanitized failure=%v", err)
	}
}

func TestAccessHTTPRejectsUnsafeSelectorsWithoutRequests(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	client, _ := NewHTTPClient(server.URL, "secret", server.Client())
	for _, key := range []string{"user@example.com?admin=true", "../users", "a%2fb@example.com", "User <a@example.com>", "a@example.com#fragment", "a\n@example.com"} {
		if _, err := client.ReadAccessUser(context.Background(), key); err == nil {
			t.Fatalf("accepted user %q", key)
		}
		if _, err := client.GetGroup(context.Background(), key); err == nil {
			t.Fatalf("accepted group %q", key)
		}
		if _, err := client.GetDirectMember(context.Background(), key, "user123"); err == nil {
			t.Fatalf("accepted group key %q", key)
		}
		if _, err := client.GetDirectMember(context.Background(), "group123", key); err == nil {
			t.Fatalf("accepted user key %q", key)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unsafe requests=%d", calls.Load())
	}
}

func TestAccessCollectorCopiesResourcesAndCapabilityRejectsSelectors(t *testing.T) {
	t.Parallel()
	client := &HTTPClient{}
	resources := map[string]string{"Engineering tools": "tools@example.com"}
	collector, err := NewAccessCollector(client, resources)
	if err != nil {
		t.Fatal(err)
	}
	resources["Engineering tools"] = "changed@example.com"
	evidence, err := collector.Collect(context.Background(), domain.Ticket{})
	result := decodeAccess(t, evidence, err)
	if result.Resources["Engineering tools"] != "tools@example.com" || collector.Capability() != AccessInspect {
		t.Fatalf("result=%+v", result)
	}
	if validateAccessInspect(json.RawMessage(`{}`)) != nil {
		t.Fatal("empty arguments rejected")
	}
	if validateAccessInspect(json.RawMessage(` { } `)) != nil {
		t.Fatal("empty arguments with whitespace rejected")
	}
	for _, input := range []string{`null`, `{"resource":"Engineering tools"}`, `{"group":"arbitrary@example.com"}`, `{} {}`} {
		if validateAccessInspect(json.RawMessage(input)) == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestGoogleProbeChecksGroupPermissionsAndEmptyResults(t *testing.T) {
	t.Parallel()
	for _, status := range []int{200, 401, 403, 404, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var users, groups atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/users" {
					users.Add(1)
				} else if r.URL.Path == "/groups" {
					groups.Add(1)
					w.WriteHeader(status)
				} else {
					t.Errorf("unexpected probe path %s", r.URL.Path)
				}
				if status == 200 || r.URL.Path == "/users" {
					_, _ = w.Write([]byte(`{}`))
				} else {
					_, _ = w.Write([]byte("secret provider body"))
				}
			}))
			defer server.Close()
			client, _ := NewHTTPClient(server.URL, "secret", server.Client())
			err := client.Probe(context.Background())
			if users.Load() != 1 || groups.Load() != 1 {
				t.Fatalf("users=%d groups=%d", users.Load(), groups.Load())
			}
			if status == 200 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			failure, ok := connectors.ProbeFailureOf(err)
			if !ok || failure != connectors.ProbeFailureForStatus(status) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("probe error=%v", err)
			}
		})
	}
}
