package jira

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestClientPostsADFCommentWithBasicAuth(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/rest/api/3/issue/HELP-42/comment" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		user, token, ok := request.BasicAuth()
		if !ok || user != "bot@example.test" || token != "test-token" {
			t.Fatalf("basic auth = %q %q %v", user, token, ok)
		}
		var body struct {
			Body struct {
				Content []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"content"`
			} `json:"body"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Body.Content) != 1 || body.Body.Content[0].Content[0].Text != "Which application?" {
			t.Fatalf("ADF body = %+v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"10001"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "bot@example.test", "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	remoteID, err := client.SendRequesterMessage(context.Background(), domain.Ticket{Source: Name, ExternalID: "HELP-42"}, domain.CaseMessage{Direction: domain.MessageOutbound, Body: "Which application?"})
	if err != nil || remoteID != "10001" {
		t.Fatalf("send = %q, %v", remoteID, err)
	}
}

func TestClientClassifiesRetryableJiraFailures(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "bot@example.test", "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SendRequesterMessage(context.Background(), domain.Ticket{Source: Name, ExternalID: "HELP-42"}, domain.CaseMessage{Direction: domain.MessageOutbound, Body: "Question"})
	var retryable interface{ Retryable() bool }
	if !errors.As(err, &retryable) || !retryable.Retryable() {
		t.Fatalf("error is not retryable: %v", err)
	}
}

func TestClientRejectsNonLoopbackHTTP(t *testing.T) {
	t.Parallel()
	if _, err := NewClient("http://jira.example.test", "bot@example.test", "token", http.DefaultClient); err == nil {
		t.Fatal("expected insecure remote Jira URL to be rejected")
	}
}
