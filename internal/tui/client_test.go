package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientSnapshotDecodesAdminResponsesAndAuthenticates(t *testing.T) {
	now := time.Date(2026, time.September, 26, 18, 30, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer operator-secret" {
			t.Errorf("Authorization = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/admin/overview":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"status": "ok", "mode": "SHADOW", "generated_at": now,
				"investigations": map[string]any{"total": 3, "pending": 1, "running": 1, "completed": 1, "failed": 0, "cancelled": 0},
				"approvals":      map[string]any{"total": 1, "pending": 1, "granted": 0, "denied": 0, "expired": 0},
				"jobs":           map[string]any{"pending": 1, "running": 1, "completed": 1, "dead": 0},
			})
		case "/v1/admin/investigations":
			if got := request.URL.Query().Get("limit"); got != "100" {
				t.Errorf("investigation limit = %q", got)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"investigations": []any{map[string]any{
				"id": "inv-1", "ticket_id": "ticket-1", "status": "RUNNING", "prompt_version": "shadow-v1", "created_at": now,
				"ticket": map[string]any{"source": "jira", "external_id": "HELP-42", "summary": "Restore access"},
			}}})
		case "/v1/admin/approvals":
			if got := request.URL.Query().Get("status"); got != "PENDING" {
				t.Errorf("approval status = %q", got)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"approvals": []any{map[string]any{
				"id": "approval-1", "action_id": "action-1", "action_digest": "digest", "status": "PENDING", "expires_at": now.Add(time.Hour),
				"investigation_id": "inv-1", "capability": "google.user.suspend", "arguments": map[string]any{"user": "alex@example.test"},
				"reason": "Account may be compromised", "policy_reason": "operator_required", "created_at": now,
			}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "operator-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := client.Snapshot(context.Background())
	if len(snapshot.Problems) != 0 {
		t.Fatalf("problems = %v", snapshot.Problems)
	}
	if snapshot.Overview.Status != "ok" || snapshot.Overview.Investigations.Running != 1 || snapshot.Overview.Approvals.Pending != 1 {
		t.Fatalf("overview = %#v", snapshot.Overview)
	}
	if len(snapshot.Investigations) != 1 || snapshot.Investigations[0].Ticket.ExternalID != "HELP-42" {
		t.Fatalf("investigations = %#v", snapshot.Investigations)
	}
	if len(snapshot.Approvals) != 1 || snapshot.Approvals[0].Capability != "google.user.suspend" {
		t.Fatalf("approvals = %#v", snapshot.Approvals)
	}
}

func TestClientDecideApprovalSendsBoundDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/admin/approvals/approval-7/decision" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("Authorization = %q", got)
		}
		var payload struct {
			Decision string `json:"decision"`
			Actor    string `json:"actor"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Decision != "GRANTED" || payload.Actor != "sam@example.test" {
			t.Errorf("payload = %#v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"approval":{"id":"approval-7"}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DecideApproval(context.Background(), "approval-7", "GRANTED", "sam@example.test"); err != nil {
		t.Fatal(err)
	}
}
