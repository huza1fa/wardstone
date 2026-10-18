package jira

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestRecoveryVerifiesOnlyMatchingBotComment(t *testing.T) {
	for _, scenario := range []string{"match", "not found", "wrong author", "foreign edit", "wrong body", "old comment", "wrong id", "bad date", "forbidden", "malformed", "oversized", "trailing JSON", "redirect", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				user, key, ok := r.BasicAuth()
				if r.Method != "GET" || !ok || user != "bot@example.test" || key != "test-token" {
					t.Errorf("unexpected method/auth")
				}
				if r.URL.Path == "/rest/api/3/myself" {
					w.Write([]byte(`{"accountId":"bot-1"}`))
					return
				}
				if r.URL.EscapedPath() != "/rest/api/3/issue/HELP%2F42/comment/10001" {
					t.Errorf("path=%s", r.URL.EscapedPath())
				}
				switch scenario {
				case "not found":
					w.WriteHeader(404)
					return
				case "forbidden":
					w.WriteHeader(403)
					w.Write([]byte("secret upstream detail"))
					return
				case "malformed":
					w.Write([]byte(`{`))
					return
				case "oversized":
					w.Write([]byte(strings.Repeat(" ", (1<<20)+1)))
					return
				case "redirect":
					w.Header().Set("Location", serverURLPlaceholder)
					w.WriteHeader(302)
					return
				}
				author, updatedAuthor, id, created, body := "bot-1", "bot-1", "10001", now.Format("2006-01-02T15:04:05.000-0700"), "Approved\nquestion <literal>"
				if scenario == "wrong author" {
					author = "requester"
				}
				if scenario == "foreign edit" {
					updatedAuthor = "requester"
				}
				if scenario == "wrong body" {
					body = "Original draft"
				}
				if scenario == "wrong id" {
					id = "other"
				}
				if scenario == "old comment" {
					created = now.Add(-time.Hour).Format(time.RFC3339)
				}
				if scenario == "bad date" {
					created = "yesterday"
				}
				json.NewEncoder(w).Encode(map[string]any{"id": id, "author": map[string]string{"accountId": author}, "updateAuthor": map[string]string{"accountId": updatedAuthor}, "created": created,
					"body": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]string{"type": "text", "text": body}}}}}})
				if scenario == "trailing JSON" {
					w.Write([]byte(`{}`))
				}
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, "bot@example.test", "test-token", server.Client())
			job := delivery.Job{Ticket: domain.Ticket{Source: Name, ExternalID: "HELP/42"}, Message: domain.CaseMessage{Direction: domain.MessageOutbound, Body: "Approved\nquestion <literal>"}, Delivery: domain.MessageDelivery{CreatedAt: now.Add(-time.Minute), ReviewedAt: &now}}
			ctx := context.Background()
			if scenario == "timeout" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			err := client.VerifyRequesterMessage(ctx, job, "10001")
			switch scenario {
			case "match":
				if err != nil || calls != 2 {
					t.Fatalf("verify=%v calls=%d", err, calls)
				}
			case "not found":
				if !errors.Is(err, delivery.ErrCommentNotFound) {
					t.Fatal(err)
				}
			default:
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatalf("unsafe result=%v", err)
				}
			}
		})
	}
}

const serverURLPlaceholder = "http://127.0.0.1:1/must-not-follow"

func TestSendFailureOutcomes(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			client, _ := NewClient(server.URL, "bot", "token", server.Client())
			_, err := client.SendRequesterMessage(context.Background(), domain.Ticket{Source: Name, ExternalID: "HELP-1"}, domain.CaseMessage{Direction: domain.MessageOutbound, Body: "Approved"})
			var outcome interface{ OutcomeUnknown() bool }
			unknown := status >= 500 || status == 408
			if !errors.As(err, &outcome) || outcome.OutcomeUnknown() != unknown {
				t.Fatalf("status=%d err=%v", status, err)
			}
		})
	}
}

func TestRecoveryRequiresMessageCorrelationForNewSends(t *testing.T) {
	for _, scenario := range []string{"match", "missing", "wrong message", "wrong digest", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/rest/api/3/myself" {
					w.Write([]byte(`{"accountId":"bot"}`))
					return
				}
				if r.URL.Query().Get("expand") != "properties" {
					t.Error("correlation property not requested")
				}
				marker := map[string]string{"message_id": "msg_1", "body_sha256": delivery.DraftDigest("Approved")}
				if scenario == "wrong message" {
					marker["message_id"] = "msg_other"
				}
				if scenario == "wrong digest" {
					marker["body_sha256"] = delivery.DraftDigest("Original")
				}
				properties := []any{map[string]any{"key": "unrelated", "value": true}}
				if scenario != "missing" {
					properties = append(properties, map[string]any{"key": "wardstone.message", "value": marker})
				}
				if scenario == "duplicate" {
					properties = append(properties, properties[1])
				}
				json.NewEncoder(w).Encode(map[string]any{"id": "10001", "properties": properties, "author": map[string]string{"accountId": "bot"}, "created": now.Format(time.RFC3339Nano), "body": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]string{"type": "text", "text": "Approved"}}}}}})
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, "bot", "token", server.Client())
			job := delivery.Job{Ticket: domain.Ticket{Source: Name, ExternalID: "HELP-1"}, Message: domain.CaseMessage{ID: "msg_1", Direction: domain.MessageOutbound, Body: "Approved"}, Delivery: domain.MessageDelivery{ReviewedAt: &now, CreatedAt: now.Add(-time.Minute), CorrelationRequired: true}}
			err := client.VerifyRequesterMessage(context.Background(), job, "10001")
			if scenario == "match" && err != nil || scenario != "match" && !errors.Is(err, delivery.ErrCommentMismatch) {
				t.Fatalf("scenario=%s err=%v", scenario, err)
			}
		})
	}
}

func TestSendIncludesMessageCorrelation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Properties []struct {
				Key   string            `json:"key"`
				Value map[string]string `json:"value"`
			} `json:"properties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Properties) != 1 || payload.Properties[0].Key != "wardstone.message" || payload.Properties[0].Value["message_id"] != "msg_1" || payload.Properties[0].Value["body_sha256"] != delivery.DraftDigest("Approved") {
			t.Errorf("correlation=%+v", payload)
		}
		w.Write([]byte(`{"id":"10001"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "bot", "token", server.Client())
	if _, err := client.SendRequesterMessage(context.Background(), domain.Ticket{Source: Name, ExternalID: "HELP-1"}, domain.CaseMessage{ID: "msg_1", Direction: domain.MessageOutbound, Body: "Approved"}); err != nil {
		t.Fatal(err)
	}
}
