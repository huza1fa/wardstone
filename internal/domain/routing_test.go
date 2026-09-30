package domain

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestRoutingDecisionValidation(t *testing.T) {
	confidence := 0.9
	valid := []RoutingDecision{
		{Specialist: "help_desk", Classification: "incident", Source: RoutingSourceRule, RuleName: "vpn", Reason: "Matched component"},
		{Specialist: "help_desk", Classification: "incident", Source: RoutingSourceModel, Confidence: &confidence, ModelProvider: "local", Model: "classifier", Reason: "Connection failure"},
		{Specialist: "help_desk", Classification: "unclassified", Source: RoutingSourceFallback, FallbackCode: "model_error", Reason: "Classification failed"},
		{Specialist: "help_desk", Classification: "unclassified", Source: RoutingSourceFallback, FallbackCode: "low_confidence", Reason: "Insufficient confidence", Confidence: &confidence, ModelProvider: "local", Model: "classifier"},
	}
	for _, decision := range valid {
		if err := decision.Validate(); err != nil {
			t.Fatalf("valid %+v: %v", decision, err)
		}
	}
	for name, mutate := range map[string]func(*RoutingDecision){
		"unknown source":        func(d *RoutingDecision) { d.Source = "unknown" },
		"blank specialist":      func(d *RoutingDecision) { d.Specialist = " \t" },
		"invalid utf8":          func(d *RoutingDecision) { d.Reason = "\xff" },
		"NUL":                   func(d *RoutingDecision) { d.Classification = "incident\x00" },
		"reason too long":       func(d *RoutingDecision) { d.Reason = strings.Repeat("a", 2001) },
		"missing rule":          func(d *RoutingDecision) { d.RuleName = "" },
		"rule model confidence": func(d *RoutingDecision) { d.Confidence = &confidence },
		"unpaired model":        func(d *RoutingDecision) { d.ModelProvider = "local" },
		"mixed fallback":        func(d *RoutingDecision) { d.FallbackCode = "unclassified" },
	} {
		t.Run(name, func(t *testing.T) {
			decision := valid[0]
			mutate(&decision)
			if err := decision.Validate(); err == nil {
				t.Fatalf("accepted invalid %+v", decision)
			}
		})
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 1.1} {
		decision := valid[1]
		decision.Confidence = &value
		if err := decision.Validate(); err == nil {
			t.Fatalf("accepted confidence %v", value)
		}
	}
	for _, mutate := range []func(*RoutingDecision){
		func(d *RoutingDecision) { d.Confidence = nil },
		func(d *RoutingDecision) { d.Model, d.ModelProvider = "", "" },
		func(d *RoutingDecision) { d.RuleName = "vpn" },
		func(d *RoutingDecision) { d.FallbackCode = "unclassified" },
	} {
		decision := valid[1]
		mutate(&decision)
		if err := decision.Validate(); err == nil {
			t.Fatalf("accepted invalid model %+v", decision)
		}
	}
	decision := valid[2]
	decision.Confidence = &confidence
	if err := decision.Validate(); err == nil {
		t.Fatal("accepted confidence without a model identity")
	}
	decision = valid[2]
	decision.FallbackCode = ""
	if err := decision.Validate(); err == nil {
		t.Fatal("accepted fallback without code")
	}
}

func TestRoutingDecisionEquality(t *testing.T) {
	a, b := 0.9, 0.9
	first := RoutingDecision{Confidence: &a}
	second := RoutingDecision{Confidence: &b}
	if !first.Equal(second) {
		t.Fatal("equal confidence values compared by pointer identity")
	}
	b = 0.8
	if first.Equal(second) || first.Equal(RoutingDecision{}) {
		t.Fatal("different confidence accepted as equal")
	}
}

func TestTicketMetadataValidationBoundaries(t *testing.T) {
	valid := TicketMetadata{IssueType: "Task", Fields: map[string][]string{"customfield_1": {"Access"}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, metadata := range map[string]TicketMetadata{
		"invalid type utf8":    {IssueType: "\xff"},
		"blank request type":   {RequestType: " \t"},
		"blank label":          {Labels: []string{""}},
		"invalid component":    {Components: []string{"\xff"}},
		"blank field":          {Fields: map[string][]string{" ": {"value"}}},
		"blank value":          {Fields: map[string][]string{"key": {" \n"}}},
		"normalized duplicate": {Fields: map[string][]string{"key": {"one"}, " key ": {"two"}}},
		"NUL field":            {Fields: map[string][]string{"key\x00": {"value"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := metadata.Validate(); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
	// Individual values and the number of fields remain within their bounds,
	// but their cumulative serialized representation must also be bounded.
	oversized := TicketMetadata{Fields: map[string][]string{}}
	for index := range 3 {
		values := make([]string, 32)
		for i := range values {
			values[i] = strings.Repeat("a", 512)
		}
		oversized.Fields[string(rune('a'+index))] = values
	}
	if err := oversized.Validate(); err == nil {
		t.Fatal("accepted cumulative metadata exceeding 32 KiB")
	}
	boundary := TicketMetadata{Fields: map[string][]string{"a": make([]string, 32), "b": make([]string, 32)}}
	for _, values := range boundary.Fields {
		for i := range values {
			values[i] = strings.Repeat("a", 512)
		}
	}
	boundary.Fields["b"][31] = "a"
	encoded, err := json.Marshal(boundary)
	if err != nil {
		t.Fatal(err)
	}
	boundary.Fields["b"][31] = strings.Repeat("a", 1+32*1024-len(encoded))
	if err := boundary.Validate(); err != nil {
		t.Fatalf("exact 32 KiB boundary rejected: %v", err)
	}
	boundary.Fields["b"][31] += "a"
	if err := boundary.Validate(); err == nil {
		t.Fatal("accepted metadata one byte beyond 32 KiB")
	}
	// Case is deliberately preserved for administrator-selected Jira keys.
	if err := (TicketMetadata{Fields: map[string][]string{"Key": {"one"}, "key": {"two"}}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTicketMetadataCloneIsolatesCollections(t *testing.T) {
	original := TicketMetadata{Components: []string{"VPN"}, Labels: []string{"it"}, Fields: map[string][]string{"key": {"value"}}}
	copy := original.Clone()
	copy.Components[0], copy.Labels[0], copy.Fields["key"][0] = "other", "other", "other"
	copy.Fields["added"] = []string{"value"}
	if original.Components[0] != "VPN" || original.Labels[0] != "it" || original.Fields["key"][0] != "value" || len(original.Fields) != 1 {
		t.Fatal("cloned metadata aliases original")
	}
	if (TicketMetadata{}).Clone().Fields != nil {
		t.Fatal("clone changed nil fields")
	}
}

func TestTicketValidationBoundaries(t *testing.T) {
	ticket := Ticket{ID: "tkt", Source: "jira", ExternalID: "TEST-1", Summary: strings.Repeat("a", 8*1024), Description: strings.Repeat("a", 64*1024)}
	if err := ticket.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Ticket){
		"summary":     func(t *Ticket) { t.Summary += "a" },
		"description": func(t *Ticket) { t.Description += "a" },
		"external ID": func(t *Ticket) { t.ExternalID = strings.Repeat("a", 257) },
		"source":      func(t *Ticket) { t.Source = " \t" },
		"email utf8":  func(t *Ticket) { t.ReporterEmail = "\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			item := ticket
			mutate(&item)
			if err := item.Validate(); err == nil {
				t.Fatal("accepted invalid ticket")
			}
		})
	}
}
