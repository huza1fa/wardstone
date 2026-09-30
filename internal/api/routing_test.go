package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/domain"
)

type fakeRoutingPreviewer struct {
	ticket domain.Ticket
	calls  int
	result domain.RoutingDecision
	err    error
}

func (f *fakeRoutingPreviewer) PreviewRouting(_ context.Context, ticket domain.Ticket) (domain.RoutingDecision, error) {
	f.ticket = ticket
	f.calls++
	return f.result, f.err
}

func TestRoutingPreviewRequiresOperatorAuthentication(t *testing.T) {
	t.Parallel()
	previewer := &fakeRoutingPreviewer{}
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret", WithRoutingPreview(previewer))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "webhook-secret", "wrong"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/admin/routing/preview", strings.NewReader(`{"external_id":"TEST-1","summary":"Review vendor"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("token %q status = %d, want 401", token, response.Code)
		}
	}
	if previewer.calls != 0 {
		t.Fatal("unauthenticated request reached preview service")
	}
}

func TestRoutingPreviewDisabledAndInvalidOption(t *testing.T) {
	t.Parallel()
	if _, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret", WithRoutingPreview(nil)); err == nil {
		t.Fatal("nil preview service was accepted")
	}
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/routing/preview", nil)
	request.Header.Set("Authorization", "Bearer operator-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestRoutingPreviewNormalizesAndReturnsDecisionWithoutIntake(t *testing.T) {
	t.Parallel()
	previewer := &fakeRoutingPreviewer{result: domain.RoutingDecision{Specialist: "vendor_review", Classification: "vendor_review", Source: "rule", RuleName: "vendor requests", Reason: "Request type matched."}}
	intake := &countingIntakeService{}
	server, err := NewServer(intake, fakeReader{}, "webhook-secret", "operator-secret", WithRoutingPreview(previewer))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return at }
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/routing/preview", strings.NewReader(`{"external_id":"TEST-1","summary":"Review vendor","request_type":"Vendor review","components":["vendor-risk"],"fields":{"risk":["high"]}}`))
	request.Header.Set("Authorization", "Bearer operator-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("preview should not be cached")
	}
	var result struct {
		Routing domain.RoutingDecision `json:"routing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Routing.Specialist != previewer.result.Specialist || result.Routing.RuleName != "vendor requests" {
		t.Fatalf("unexpected decision: %+v", result)
	}
	if previewer.calls != 1 || previewer.ticket.Source != jira.Name || previewer.ticket.CreatedAt != at || previewer.ticket.Metadata.RequestType != "Vendor review" || previewer.ticket.Metadata.Fields["risk"][0] != "high" {
		t.Fatalf("unexpected normalized ticket: %+v", previewer.ticket)
	}
	if intake.calls != 0 {
		t.Fatal("preview created an investigation")
	}
}

func TestTicketPayloadBoundariesForPreviewAndIntake(t *testing.T) {
	t.Parallel()
	valid := `{"external_id":"TEST-1","summary":"Review vendor"}`
	tests := []struct {
		name, body string
		status     int
	}{
		{"empty", "", http.StatusBadRequest},
		{"null", "null", http.StatusBadRequest},
		{"array", "[]", http.StatusBadRequest},
		{"unknown field", `{"external_id":"TEST-1","summary":"x","permissions":["admin"]}`, http.StatusBadRequest},
		{"nested metadata", `{"external_id":"TEST-1","summary":"x","fields":{"risk":{"value":"high"}}}`, http.StatusBadRequest},
		{"empty metadata value", `{"external_id":"TEST-1","summary":"x","components":[""]}`, http.StatusBadRequest},
		{"empty custom field values", `{"external_id":"TEST-1","summary":"x","fields":{"risk":[]}}`, http.StatusBadRequest},
		{"oversized request type", `{"external_id":"TEST-1","summary":"x","request_type":"` + strings.Repeat("x", 257) + `"}`, http.StatusBadRequest},
		{"missing summary", `{"external_id":"TEST-1"}`, http.StatusBadRequest},
		{"multiple objects", valid + valid, http.StatusBadRequest},
		{"trailing invalid", valid + "garbage", http.StatusBadRequest},
		{"oversized description", `{"external_id":"TEST-1","summary":"x","description":"` + strings.Repeat("x", maxRequestBody) + `"}`, http.StatusRequestEntityTooLarge},
		{"oversized whitespace", valid + strings.Repeat(" ", maxRequestBody), http.StatusRequestEntityTooLarge},
	}
	for _, endpoint := range []struct{ path, token string }{{"/v1/admin/routing/preview", "operator-secret"}, {"/v1/tickets/jira", "webhook-secret"}} {
		for _, test := range tests {
			t.Run(endpoint.path+"/"+test.name, func(t *testing.T) {
				previewer := &fakeRoutingPreviewer{}
				intake := &countingIntakeService{}
				server, err := NewServer(intake, fakeReader{}, "webhook-secret", "operator-secret", WithRoutingPreview(previewer))
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(test.body))
				request.Header.Set("Authorization", "Bearer "+endpoint.token)
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
				}
				if previewer.calls != 0 || intake.calls != 0 {
					t.Fatal("invalid payload reached service")
				}
			})
		}
	}
}

func TestRoutingPreviewDoesNotExposeInternalErrors(t *testing.T) {
	t.Parallel()
	previewer := &fakeRoutingPreviewer{err: errors.New("private database credential")}
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret", WithRoutingPreview(previewer))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/routing/preview", strings.NewReader(`{"external_id":"TEST-1","summary":"Review vendor"}`))
	request.Header.Set("Authorization", "Bearer operator-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "credential") {
		t.Fatalf("unsafe error response: %d %s", response.Code, response.Body.String())
	}
}

type countingIntakeService struct{ calls int }

func (f *countingIntakeService) Receive(context.Context, domain.Ticket) (domain.InvestigationID, bool, error) {
	f.calls++
	return "inv_1", true, nil
}
