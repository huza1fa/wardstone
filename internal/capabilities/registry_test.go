package capabilities

import (
	"encoding/json"
	"github.com/wardstone-project/wardstone/internal/domain"
	"testing"
)

func TestArgumentSchemasAreImmutableMetadata(t *testing.T) {
	input := json.RawMessage(`{"type":"object"}`)
	registry := NewRegistry()
	if err := registry.Register(Definition{Name: "read", Connector: "test", Description: "Read", Effect: domain.EffectRead, ArgumentsVersion: 1, ArgumentsSchema: input}); err != nil {
		t.Fatal(err)
	}
	input[0] = '['
	first, err := registry.Get("read")
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(first.ArgumentsSchema) {
		t.Fatal("registration retained caller-owned schema bytes")
	}
	first.ArgumentsSchema[0] = '['
	second, _ := registry.Get("read")
	if !json.Valid(second.ArgumentsSchema) {
		t.Fatal("caller could mutate registry schema")
	}
}
