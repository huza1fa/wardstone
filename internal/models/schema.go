package models

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/domain"
)

// The wrapper keeps the schema root an object while expressing disjoint result
// shapes in a nested anyOf. Runtime citation and authority checks still apply.
func diagnosisSchema(request agent.Request) map[string]any {
	text := map[string]any{"type": "string"}
	strings := map[string]any{"type": "array", "items": text}
	evidenceIDs := make([]domain.EvidenceID, 0, len(request.Evidence))
	for _, item := range request.Evidence {
		evidenceIDs = append(evidenceIDs, item.ID)
	}
	if len(evidenceIDs) == 0 {
		evidenceIDs = []domain.EvidenceID{"__no_supplied_evidence__"}
	}
	citations := map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": evidenceIDs}}
	object := func(properties map[string]any) map[string]any {
		required := make([]string, 0, len(properties))
		// Sorted property names make provider payloads reproducible.
		for key := range properties {
			required = append(required, key)
		}
		sort.Strings(required)
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	finding := object(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"verified", "vendor_claim", "assumption"}}, "statement": text, "evidence_ids": citations})
	reportProperties := map[string]any{
		"version": map[string]any{"type": "integer", "enum": []int{1}}, "summary": text,
		"findings": map[string]any{"type": "array", "items": finding}, "unknowns": strings,
		"escalation_reason": text, "requester_summary": text, "expected_impact": text,
	}

	// Registered argument schemas produce exact objects. Capabilities without a
	// schema use JSON text, decoded and validated by the capability registry.
	action := object(map[string]any{"capability": text, "arguments_json": text, "reason": text, "evidence_ids": citations})
	var actionVariants []any
	for _, contract := range request.ActionContracts {
		permitted := false
		for _, name := range request.PermittedActions {
			if name == contract.Capability {
				permitted = true
			}
		}
		if !permitted {
			continue
		}
		var args map[string]any
		if json.Unmarshal(contract.ArgumentsSchema, &args) != nil {
			continue
		}
		actionVariants = append(actionVariants, object(map[string]any{"capability": map[string]any{"type": "string", "enum": []domain.CapabilityName{contract.Capability}}, "arguments": args, "reason": text, "evidence_ids": citations}))
	}
	if len(actionVariants) == 1 {
		action = actionVariants[0].(map[string]any)
	} else if len(actionVariants) > 1 {
		action = map[string]any{"anyOf": actionVariants}
	}
	question := object(map[string]any{"follow_up_question": text})
	final := object(map[string]any{"diagnosis": text, "actions": map[string]any{"type": "array", "items": action}, "work_package": object(reportProperties)})
	if request.Specialist == "vendor_review" {
		ids := []string{}
		if request.VendorReview != nil && request.VendorReview.Rubric != nil {
			for _, c := range request.VendorReview.Rubric.Criteria {
				ids = append(ids, c.ID)
			}
		}
		limit := len(ids)
		if len(ids) == 0 {
			ids = []string{"__rubric_unconfigured__"}
		}
		criterion := object(map[string]any{"id": map[string]any{"type": "string", "enum": ids}, "status": map[string]any{"type": "string", "enum": []string{"met", "partial", "unmet", "unknown"}}, "reason": text, "evidence_ids": citations})
		final["properties"].(map[string]any)["vendor_assessment"] = object(map[string]any{"criteria": map[string]any{"type": "array", "items": criterion, "maxItems": limit}})
		final["required"] = []string{"diagnosis", "actions", "work_package", "vendor_assessment"}
	}
	variants := []any{question, final}
	if request.ResponseConstraint == "final_report" {
		variants = []any{final}
	}
	if request.ResponseConstraint == "requester_question" {
		variants = []any{question}
	}
	if len(request.HandoffTargets) > 0 && request.ResponseConstraint == "" {
		variants = append(variants, object(map[string]any{"handoff": object(map[string]any{"specialist": map[string]any{"type": "string", "enum": request.HandoffTargets}, "reason": text})}))
	}
	return object(map[string]any{"result": map[string]any{"anyOf": variants}})
}

func decodeSchemaResult(content string) (agent.Result, error) {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := decodeObject(content, &envelope, "result"); err != nil {
		return agent.Result{}, err
	}
	var wire struct {
		Diagnosis        string `json:"diagnosis"`
		FollowUpQuestion string `json:"follow_up_question"`
		Actions          []struct {
			Capability    domain.CapabilityName `json:"capability"`
			ArgumentsJSON string                `json:"arguments_json"`
			Arguments     json.RawMessage       `json:"arguments"`
			Reason        string                `json:"reason"`
			EvidenceIDs   []domain.EvidenceID   `json:"evidence_ids"`
		} `json:"actions"`
		Handoff          *agent.Handoff           `json:"handoff"`
		WorkPackage      *domain.WorkPackage      `json:"work_package"`
		VendorAssessment *domain.VendorAssessment `json:"vendor_assessment"`
	}
	if err := decodeObject(string(envelope.Result), &wire, "diagnosis", "follow_up_question", "actions", "handoff", "work_package", "vendor_assessment"); err != nil {
		return agent.Result{}, err
	}
	result := agent.Result{Diagnosis: wire.Diagnosis, FollowUpQuestion: wire.FollowUpQuestion, Handoff: wire.Handoff, WorkPackage: wire.WorkPackage, VendorAssessment: wire.VendorAssessment}
	for _, action := range wire.Actions {
		arguments := action.Arguments
		if len(arguments) > 0 && action.ArgumentsJSON != "" {
			return agent.Result{}, errors.New("schema action contains conflicting argument encodings")
		}
		if len(arguments) == 0 {
			arguments = json.RawMessage(action.ArgumentsJSON)
		}
		if !json.Valid(arguments) {
			return agent.Result{}, errors.New("schema action contains invalid argument JSON")
		}
		result.Actions = append(result.Actions, agent.ProposedAction{Capability: action.Capability, Arguments: arguments, Reason: action.Reason, EvidenceIDs: action.EvidenceIDs})
	}
	return result, nil
}
