package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wardstone-project/wardstone/internal/agent"
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
