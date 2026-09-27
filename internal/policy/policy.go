package policy

import (
	"context"

	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/domain"
)

type RuleMode string

const (
	RuleAllow    RuleMode = "allow"
	RuleApproval RuleMode = "approval"
	RuleDeny     RuleMode = "deny"
)

type Evaluator struct {
	mode     domain.OperatingMode
	registry *capabilities.Registry
	rules    map[domain.CapabilityName]RuleMode
}

func New(mode domain.OperatingMode, registry *capabilities.Registry, rules map[domain.CapabilityName]RuleMode) *Evaluator {
	cloned := make(map[domain.CapabilityName]RuleMode, len(rules))
	for name, rule := range rules {
		cloned[name] = rule
	}
	return &Evaluator{mode: mode, registry: registry, rules: cloned}
}

func (e *Evaluator) Evaluate(_ context.Context, action domain.ProposedAction) domain.PolicyResult {
	definition, err := e.registry.Get(action.Capability)
	if err != nil {
		return domain.PolicyResult{Decision: domain.PolicyDeny, Reason: "unknown_capability"}
	}
	if err := e.registry.ValidateAction(action); err != nil {
		return domain.PolicyResult{Decision: domain.PolicyDeny, Reason: "invalid_action"}
	}
	if definition.Effect == domain.EffectMutate && e.mode == domain.OperatingModeShadow {
		return domain.PolicyResult{Decision: domain.PolicyDeny, Reason: "shadow_mode"}
	}
	rule, exists := e.rules[action.Capability]
	if !exists {
		return domain.PolicyResult{Decision: domain.PolicyDeny, Reason: "no_matching_rule"}
	}
	switch rule {
	case RuleDeny:
		return domain.PolicyResult{Decision: domain.PolicyDeny, Reason: "policy_denied"}
	case RuleApproval:
		return domain.PolicyResult{Decision: domain.PolicyRequireApproval, Reason: "policy_requires_approval"}
	case RuleAllow:
		if definition.Effect == domain.EffectMutate && e.mode == domain.OperatingModeApproval {
			return domain.PolicyResult{Decision: domain.PolicyRequireApproval, Reason: "approval_mode"}
		}
		return domain.PolicyResult{Decision: domain.PolicyAllow, Reason: "policy_allowed"}
	default:
		return domain.PolicyResult{Decision: domain.PolicyDeny, Reason: "invalid_policy_rule"}
	}
}
