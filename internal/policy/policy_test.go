package policy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestShadowModeDeniesMutationRegardlessOfRule(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	evaluator := New(domain.OperatingModeShadow, registry, map[domain.CapabilityName]RuleMode{"test.write": RuleAllow})
	result := evaluator.Evaluate(context.Background(), sealedAction(t, "test.write"))
	if result.Decision != domain.PolicyDeny || result.Reason != "shadow_mode" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestShadowModeAllowsExplicitRead(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	evaluator := New(domain.OperatingModeShadow, registry, map[domain.CapabilityName]RuleMode{"test.read": RuleAllow})
	result := evaluator.Evaluate(context.Background(), sealedAction(t, "test.read"))
	if result.Decision != domain.PolicyAllow {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestUnknownCapabilityIsDenied(t *testing.T) {
	t.Parallel()
	evaluator := New(domain.OperatingModeShadow, testRegistry(t), nil)
	result := evaluator.Evaluate(context.Background(), sealedAction(t, "unknown"))
	if result.Decision != domain.PolicyDeny || result.Reason != "unknown_capability" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func BenchmarkEvaluate(b *testing.B) {
	registry := capabilities.NewRegistry()
	_ = registry.Register(capabilities.Definition{Name: "test.read", Connector: "test", Description: "read", Effect: domain.EffectRead, ArgumentsVersion: 1})
	evaluator := New(domain.OperatingModeShadow, registry, map[domain.CapabilityName]RuleMode{"test.read": RuleAllow})
	action := sealedAction(b, "test.read")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = evaluator.Evaluate(context.Background(), action)
	}
}

type fataler interface {
	Helper()
	Fatal(...any)
}

func sealedAction(t fataler, name domain.CapabilityName) domain.ProposedAction {
	t.Helper()
	action := domain.ProposedAction{ID: "act_1", InvestigationID: "inv_1", Capability: name, Arguments: json.RawMessage(`{}`)}
	if err := action.Seal(); err != nil {
		t.Fatal(err)
	}
	return action
}

func testRegistry(t *testing.T) *capabilities.Registry {
	t.Helper()
	registry := capabilities.NewRegistry()
	err := registry.Register(
		capabilities.Definition{Name: "test.read", Connector: "test", Description: "read", Effect: domain.EffectRead, ArgumentsVersion: 1},
		capabilities.Definition{Name: "test.write", Connector: "test", Description: "write", Effect: domain.EffectMutate, ArgumentsVersion: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
