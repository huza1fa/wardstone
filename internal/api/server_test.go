package api

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestJiraReplyWebhookAndConversationReadsUseSeparateTokens(t *testing.T) {
	t.Parallel()
	conversations := &fakeConversationService{}
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret", WithConversations(conversations))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tickets/jira/replies", bytes.NewBufferString(`{"external_id":"HELP-1","comment_id":"10001","author":"requester@example.com","body":"The asset tag is LT-1042"}`))
	request.Header.Set("Authorization", "Bearer webhook-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || conversations.ticketExternalID != "HELP-1" || conversations.message.ExternalID != "10001" {
		t.Fatalf("reply status=%d service=%+v", response.Code, conversations)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/investigations/inv_1/messages", nil)
	request.Header.Set("Authorization", "Bearer webhook-secret")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("webhook token read status = %d, want 401", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/investigations/inv_1/messages", nil)
	request.Header.Set("Authorization", "Bearer operator-secret")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("operator conversation read status = %d", response.Code)
	}
	var body struct {
		Messages []domain.CaseMessage `json:"messages"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || len(body.Messages) != 1 {
		t.Fatalf("conversation response = %+v, err=%v", body, err)
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

type fakeConversationService struct {
	ticketExternalID string
	message          domain.CaseMessage
}

func (f *fakeConversationService) ReceiveRequesterReply(_ context.Context, _ domain.ConnectorName, ticketExternalID string, message domain.CaseMessage) (domain.InvestigationID, bool, error) {
	f.ticketExternalID, f.message = ticketExternalID, message
	return "inv_1", true, nil
}

func (f *fakeConversationService) ListMessages(context.Context, domain.InvestigationID) ([]domain.CaseMessage, error) {
	return []domain.CaseMessage{{ID: "msg_1", InvestigationID: "inv_1", Direction: domain.MessageOutbound, Body: "Which device?"}}, nil
}
