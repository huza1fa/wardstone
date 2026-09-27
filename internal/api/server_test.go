package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestInvestigationReadsRequireOperatorToken(t *testing.T) {
	t.Parallel()
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/investigations/inv_1", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/investigations/inv_1", nil)
	request.Header.Set("Authorization", "Bearer operator-secret")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}

func TestJiraWebhookDoesNotAcceptOperatorToken(t *testing.T) {
	t.Parallel()
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tickets/jira", nil)
	request.Header.Set("Authorization", "Bearer operator-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

type fakeService struct{}

func (fakeService) Receive(context.Context, domain.Ticket) (domain.InvestigationID, bool, error) {
	return "inv_1", true, nil
}

type fakeReader struct{}

func (fakeReader) GetInvestigation(context.Context, domain.InvestigationID) (domain.Investigation, error) {
	return domain.Investigation{ID: "inv_1", Status: domain.InvestigationPending}, nil
}

func (fakeReader) Timeline(context.Context, domain.InvestigationID) ([]audit.Event, error) {
	return nil, nil
}
