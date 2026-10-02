package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wardstone-project/wardstone/internal/connectors"
)

func TestProbeUsesBoundedReadOnlyDirectoryRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/users" || request.URL.Query().Get("customer") != "my_customer" || request.URL.Query().Get("maxResults") != "1" || request.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("unexpected request: %s %s auth=%q", request.Method, request.URL.String(), request.Header.Get("Authorization"))
		}
		_, _ = writer.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "access-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProbeRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(make([]byte, (64<<10)+1))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "access-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if failure, ok := connectors.ProbeFailureOf(err); !ok || failure != connectors.ProbeInvalidResponse {
		t.Fatalf("probe error = %v", err)
	}
}

func TestProbeRejectsWrongSuccessfulResponseShape(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{"missing": `{}`, "null": `{"users":null}`, "object": `{"users":{}}`, "string": `{"users":"error"}`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()
			client, err := NewHTTPClient(server.URL, "access-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = client.Probe(context.Background())
			if failure, ok := connectors.ProbeFailureOf(err); !ok || failure != connectors.ProbeInvalidResponse {
				t.Fatalf("probe error = %v", err)
			}
		})
	}
}

func TestClientRejectsUnsafeBaseURL(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"http://google.example.test", "https://user:secret@google.example.test", "https://google.example.test?token=secret"} {
		if _, err := NewHTTPClient(endpoint, "token", http.DefaultClient); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", endpoint)
		}
	}
}

func TestProbeDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("token followed redirect") }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "access-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if failure, ok := connectors.ProbeFailureOf(err); !ok || failure != connectors.ProbeRedirectBlocked {
		t.Fatalf("probe error = %v", err)
	}
}
