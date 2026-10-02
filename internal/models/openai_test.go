package models

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/connectors"
)

func TestOpenAICompatibleRejectsTrailingModelJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"{\"diagnosis\":\"ok\",\"actions\":[]} {}"}}]}`))
	}))
	defer server.Close()
	provider, err := NewOpenAICompatible(server.URL, "", "test", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Diagnose(context.Background(), agent.Request{}); err == nil {
		t.Fatal("expected trailing model JSON to be rejected")
	}
}

func TestProbeReadsConfiguredModelMetadata(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.EscapedPath() != "/models/team%2Fmodel" || request.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected probe: %s %s auth=%q", request.Method, request.URL.EscapedPath(), request.Header.Get("Authorization"))
		}
		_, _ = writer.Write([]byte(`{"id":"team/model"}`))
	}))
	defer server.Close()
	provider, err := NewOpenAICompatible(server.URL, "test-key", "team/model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProbeClassifiesMissingModel(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte("private provider response"))
	}))
	defer server.Close()
	provider, err := NewOpenAICompatible(server.URL, "", "missing", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Probe(context.Background())
	if failure, ok := connectors.ProbeFailureOf(err); !ok || failure != connectors.ProbeNotFound || err.Error() != "not_found" {
		t.Fatalf("probe error = %v", err)
	}
}

func TestProbeRejectsMismatchedModelMetadata(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"id":"different-model"}`))
	}))
	defer server.Close()
	provider, err := NewOpenAICompatible(server.URL, "", "expected-model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Probe(context.Background())
	if failure, ok := connectors.ProbeFailureOf(err); !ok || failure != connectors.ProbeInvalidResponse {
		t.Fatalf("probe error = %v", err)
	}
}

func TestProviderRejectsUnsafeBaseURL(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"http://model.example.test", "https://user:secret@model.example.test", "https://model.example.test#fragment"} {
		if _, err := NewOpenAICompatible(endpoint, "key", "model", http.DefaultClient); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", endpoint)
		}
	}
}

func TestIntentModelRejectsAmbiguousOrMalformedDecisions(t *testing.T) {
	t.Parallel()
	valid := `{"specialist":"help_desk","classification":"help_desk","confidence":0,"reason":"Unclear request"}`
	for _, test := range []struct {
		name, content string
		valid         bool
	}{
		{"zero confidence", valid, true},
		{"null", `null`, false},
		{"array", `[]`, false},
		{"missing confidence", `{"specialist":"help_desk","classification":"help_desk","reason":"unclear"}`, false},
		{"null confidence", `{"specialist":"help_desk","classification":"help_desk","confidence":null,"reason":"unclear"}`, false},
		{"duplicate role", `{"specialist":"help_desk","specialist":"access_management","classification":"help_desk","confidence":0.9,"reason":"unclear"}`, false},
		{"case duplicate role", `{"specialist":"help_desk","Specialist":"access_management","classification":"help_desk","confidence":0.9,"reason":"unclear"}`, false},
		{"case duplicate confidence", `{"specialist":"help_desk","classification":"help_desk","confidence":0.1,"Confidence":0.9,"reason":"unclear"}`, false},
		{"duplicate confidence", `{"specialist":"help_desk","classification":"help_desk","confidence":0.1,"confidence":0.9,"reason":"unclear"}`, false},
		{"extra permission", `{"specialist":"help_desk","classification":"help_desk","confidence":0.9,"reason":"help","tools":["google.users.delete"]}`, false},
		{"trailing object", valid + ` {}`, false},
		{"malformed", `{"confidence":`, false},
		{"overflow", `{"specialist":"help_desk","classification":"help_desk","confidence":1e999,"reason":"help"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
					t.Errorf("request=%s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("missing configured authorization")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": test.content}}}})
			}))
			defer server.Close()
			provider, err := NewOpenAICompatible(server.URL, "test-key", "test", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := provider.ClassifyIntent(context.Background(), agent.IntentRequest{})
			if (err == nil) != test.valid {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestIntentModelResponseBudgetAndErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"too large", http.StatusOK, strings.Repeat("x", (16<<10)+1)},
		{"vendor error", http.StatusForbidden, "private vendor failure body"},
		{"no choices", http.StatusOK, `{"choices":[]}`},
		{"multiple choices", http.StatusOK, `{"choices":[{"message":{"content":"{}"}},{"message":{"content":"{}"}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			provider, err := NewOpenAICompatible(server.URL, "", "test", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.ClassifyIntent(context.Background(), agent.IntentRequest{})
			if err == nil {
				t.Fatal("expected invalid response rejection")
			}
			if strings.Contains(err.Error(), "private vendor") {
				t.Fatal("vendor response body leaked through error")
			}
		})
	}
}

func TestIntentModelCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cancelled request reached model") }))
	defer server.Close()
	provider, err := NewOpenAICompatible(server.URL, "", "test", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.ClassifyIntent(ctx, agent.IntentRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestModelRedirectDoesNotForwardTicketOrCredentials(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("model redirect forwarded private request") }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
			defer server.Close()
			client := server.Client()
			provider, err := NewOpenAICompatible(server.URL, "test-key", "test", client)
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.ClassifyIntent(context.Background(), agent.IntentRequest{})
			if err == nil {
				t.Fatal("redirect should be rejected")
			}
			if client.CheckRedirect != nil {
				t.Fatal("provider modified shared connector client")
			}
		})
	}
}
