package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

type RoutingSource string

const (
	RoutingSourceRule     RoutingSource = "rule"
	RoutingSourceModel    RoutingSource = "model"
	RoutingSourceFallback RoutingSource = "fallback"
)

// RoutingDecision records the original dispatch independently of the current
// specialist, which can change during an investigation handoff.
type RoutingDecision struct {
	Specialist     SpecialistName    `json:"specialist"`
	Classification string            `json:"classification"`
	Source         RoutingSource     `json:"source"`
	RuleName       string            `json:"rule_name,omitempty"`
	Reason         string            `json:"reason"`
	Confidence     *float64          `json:"confidence,omitempty"`
	FallbackCode   string            `json:"fallback_code,omitempty"`
	ModelProvider  ModelProviderName `json:"model_provider,omitempty"`
	Model          string            `json:"model,omitempty"`
}

func (d RoutingDecision) Validate() error {
	for _, field := range []struct {
		name     string
		value    string
		limit    int
		required bool
	}{
		{"specialist", string(d.Specialist), 128, true},
		{"classification", d.Classification, 128, true},
		{"reason", d.Reason, 2000, true},
		{"rule name", d.RuleName, 128, false},
		{"fallback code", d.FallbackCode, 128, false},
		{"model provider", string(d.ModelProvider), 128, false},
		{"model", d.Model, 256, false},
	} {
		if err := validateText(field.name, field.value, field.limit, field.required); err != nil {
			return err
		}
	}
	if d.Confidence != nil && (math.IsNaN(*d.Confidence) || math.IsInf(*d.Confidence, 0) || *d.Confidence < 0 || *d.Confidence > 1) {
		return errors.New("routing confidence must be finite and between zero and one")
	}
	if (d.ModelProvider == "") != (d.Model == "") {
		return errors.New("routing model provider and model must be supplied together")
	}
	switch d.Source {
	case RoutingSourceRule:
		if d.RuleName == "" || d.Confidence != nil || d.FallbackCode != "" || d.ModelProvider != "" {
			return errors.New("rule routing requires a rule name and cannot contain model or fallback fields")
		}
	case RoutingSourceModel:
		if d.Confidence == nil || d.ModelProvider == "" || d.RuleName != "" || d.FallbackCode != "" {
			return errors.New("model routing requires confidence and model identity and cannot contain rule or fallback fields")
		}
	case RoutingSourceFallback:
		if d.FallbackCode == "" || d.RuleName != "" || (d.Confidence != nil && d.ModelProvider == "") {
			return errors.New("fallback routing requires a fallback code; model confidence requires model identity")
		}
	default:
		return errors.New("unknown routing source")
	}
	return nil
}

// Equal compares confidence values rather than their pointer identities.
func (d RoutingDecision) Equal(other RoutingDecision) bool {
	if (d.Confidence == nil) != (other.Confidence == nil) {
		return false
	}
	if d.Confidence != nil && *d.Confidence != *other.Confidence {
		return false
	}
	d.Confidence, other.Confidence = nil, nil
	return d == other
}

func validateText(name, value string, limit int, required bool) error {
	if len(value) > limit || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s must be valid UTF-8 without NUL and at most %d bytes", name, limit)
	}
	if strings.TrimSpace(value) == "" && (required || value != "") {
		return fmt.Errorf("%s must not be blank", name)
	}
	return nil
}
