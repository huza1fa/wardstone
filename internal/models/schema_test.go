package models

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestStructuredOutputUsesDisjointSchemaAndValidatedArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResponseFormat struct {
				Type   string `json:"type"`
				Schema struct {
					Strict bool           `json:"strict"`
					Schema map[string]any `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ResponseFormat.Type != "json_schema" || !body.ResponseFormat.Schema.Strict {
			t.Error("structured output was not strict")
		}
		root := body.ResponseFormat.Schema.Schema
		variants := root["properties"].(map[string]any)["result"].(map[string]any)["anyOf"].([]any)
		if len(variants) != 2 {
			t.Error("handoff shape should not be offered without allowed targets")
		}
		content := `{"result":{"diagnosis":"Review needed","actions":[{"capability":"test.read","arguments_json":"{\"user\":\"person@example.test\"}","reason":"Inspect identity","evidence_ids":[]}],"work_package":{"version":1,"summary":"Review needed","findings":[],"unknowns":[],"escalation_reason":"","requester_summary":"Review pending","expected_impact":"Read identity"}}}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer server.Close()
	p, err := NewOpenAICompatible(server.URL, "", "test", server.Client(), Options{StructuredOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Diagnose(context.Background(), agent.Request{})
	if err != nil || result.WorkPackage == nil || len(result.Actions) != 1 || string(result.Actions[0].Arguments) != `{"user":"person@example.test"}` {
		t.Fatalf("structured diagnosis=%+v err=%v", result, err)
	}
}

func TestStructuredWireRejectsInvalidAndDuplicateValues(t *testing.T) {
	for _, content := range []string{
		`{"result":{"follow_up_question":"Which vendor?"},"result":{"follow_up_question":"Override"}}`,
		`{"result":{"actions":[{"capability":"read","arguments_json":"invalid JSON","reason":"a","evidence_ids":[]}]}}`,
		`{"result":{"permissions":["root"]}}`,
		`{"result":{"work_package":{"version":1,"summary":"Review","risk_assessment":{"rating":"LOW","rating":"HIGH","rationale":"a","evidence_ids":["d"]}}}}`,
	} {
		if _, err := decodeSchemaResult(content); err == nil {
			t.Fatal("invalid schema wire accepted")
		}
	}
	schema := diagnosisSchema(agent.Request{Specialist: "vendor_review", HandoffTargets: []domain.SpecialistName{"help_desk"}})
	properties := schema["properties"].(map[string]any)["result"].(map[string]any)["anyOf"].([]any)
	if len(properties) != 3 {
		t.Fatal("missing configured handoff shape")
	}
	report := properties[1].(map[string]any)["properties"].(map[string]any)["work_package"].(map[string]any)["properties"].(map[string]any)
	if report["risk_assessment"] != nil {
		t.Fatal("schema offered a model-generated rating")
	}
	final := properties[1].(map[string]any)["properties"].(map[string]any)
	assessment := final["vendor_assessment"].(map[string]any)["properties"].(map[string]any)["criteria"].(map[string]any)
	if assessment["maxItems"] != 0 {
		t.Fatal("unconfigured rubric permitted criterion assessments")
	}

}

func TestSchemaConstrainsKnownWorkflowBoundaries(t *testing.T) {
	for _, constraint := range []string{"final_report", "requester_question"} {
		schema := diagnosisSchema(agent.Request{ResponseConstraint: constraint, HandoffTargets: []domain.SpecialistName{"help_desk"}})
		variants := schema["properties"].(map[string]any)["result"].(map[string]any)["anyOf"].([]any)
		if len(variants) != 1 {
			t.Fatalf("%s offered competing responses", constraint)
		}
		fields := variants[0].(map[string]any)["properties"].(map[string]any)
		if constraint == "final_report" && fields["work_package"] == nil {
			t.Fatal("operator boundary lacks report")
		}
		if constraint == "requester_question" && fields["follow_up_question"] == nil {
			t.Fatal("missing-selector boundary lacks question")
		}
		if fields["handoff"] != nil {
			t.Fatal("known workflow boundary could bounce between specialists")
		}
	}
}

func TestRegisteredActionSchemaUsesExactArgumentNames(t *testing.T) {
	contract := agent.ActionContract{Capability: "google.groups.add_member", ArgumentsVersion: 1, ArgumentsSchema: json.RawMessage(`{"type":"object","properties":{"group":{"type":"string"},"user":{"type":"string"}},"required":["group","user"],"additionalProperties":false}`)}
	schema := diagnosisSchema(agent.Request{PermittedActions: []domain.CapabilityName{contract.Capability}, ActionContracts: []agent.ActionContract{contract}})
	variants := schema["properties"].(map[string]any)["result"].(map[string]any)["anyOf"].([]any)
	report := variants[1].(map[string]any)["properties"].(map[string]any)
	action := report["actions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	fields := action["arguments"].(map[string]any)["properties"].(map[string]any)
	if fields["group"] == nil || fields["user"] == nil || fields["group_email"] != nil || action["arguments_json"] != nil {
		t.Fatal("registered argument contract was not enforced by schema")
	}
	result, err := decodeSchemaResult(`{"result":{"actions":[{"capability":"google.groups.add_member","arguments":{"group":"tools@example.test","user":"person@example.test"},"reason":"Missing membership","evidence_ids":["doc"]}]}}`)
	if err != nil || len(result.Actions) != 1 || string(result.Actions[0].Arguments) != `{"group":"tools@example.test","user":"person@example.test"}` {
		t.Fatalf("typed action decode=%+v err=%v", result, err)
	}
	if _, err := decodeSchemaResult(`{"result":{"actions":[{"capability":"write","arguments":{},"arguments_json":"{}","reason":"x","evidence_ids":[]}]}}`); err == nil {
		t.Fatal("conflicting argument encodings accepted")
	}
}

func TestStructuredCitationsUseOnlySuppliedEvidenceIDs(t *testing.T) {
	request := agent.Request{Specialist: "vendor_review", Evidence: []domain.Evidence{
		{ID: "evd_context", Kind: "research.request_context"},
		{ID: "evd_document", Kind: "research.vendor_document"},
	}}
	variants := diagnosisSchema(request)["properties"].(map[string]any)["result"].(map[string]any)["anyOf"].([]any)
	report := variants[1].(map[string]any)["properties"].(map[string]any)["work_package"].(map[string]any)["properties"].(map[string]any)
	finding := report["findings"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	assessment := variants[1].(map[string]any)["properties"].(map[string]any)["vendor_assessment"].(map[string]any)["properties"].(map[string]any)["criteria"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for _, citations := range []any{finding["evidence_ids"], assessment["evidence_ids"]} {
		ids := citations.(map[string]any)["items"].(map[string]any)["enum"].([]domain.EvidenceID)
		if len(ids) != 2 || ids[0] != "evd_context" || ids[1] != "evd_document" {
			t.Fatalf("citation schema is not bounded to supplied evidence: %v", ids)
		}
	}
}

func TestVendorSchemaOffersAssessmentsWithoutRatingAuthority(t *testing.T) {
	request := agent.Request{Specialist: "vendor_review", VendorReview: &domain.VendorReviewContext{Vendor: "acme", Rubric: &domain.VendorRubric{Version: "fixture-v1", ModerateAt: 25, HighAt: 60, Criteria: []domain.VendorCriterion{{ID: "identity", Requirement: "Identity controls", Weight: 100, Evidence: "vendor_claim"}}}}}
	variants := diagnosisSchema(request)["properties"].(map[string]any)["result"].(map[string]any)["anyOf"].([]any)
	final := variants[1].(map[string]any)["properties"].(map[string]any)
	report := final["work_package"].(map[string]any)["properties"].(map[string]any)
	if report["risk_assessment"] != nil {
		t.Fatal("model can choose overall risk")
	}
	criteria := final["vendor_assessment"].(map[string]any)["properties"].(map[string]any)["criteria"].(map[string]any)
	fields := criteria["items"].(map[string]any)["properties"].(map[string]any)
	ids := fields["id"].(map[string]any)["enum"].([]string)
	if criteria["maxItems"] != 1 || len(ids) != 1 || ids[0] != "identity" || fields["score"] != nil {
		t.Fatal("assessment schema is not bounded to configured criteria")
	}
	for _, structured := range []bool{false, true} {
		for _, scenario := range []string{"valid", "overall rating", "duplicate ID", "question mixed with assessment"} {
			t.Run(fmt.Sprintf("structured=%v/%s", structured, scenario), func(t *testing.T) {
				content := `{"work_package":{"version":1,"summary":"Source review"},"vendor_assessment":{"criteria":[{"id":"identity","status":"unknown","reason":"Missing evidence","evidence_ids":[]}]}}`
				switch scenario {
				case "overall rating":
					content = `{"work_package":{"version":1,"summary":"Review","risk_assessment":{"rating":"UNKNOWN","rationale":"No evidence","evidence_ids":[]}},"vendor_assessment":{"criteria":[]}}`
				case "duplicate ID":
					content = `{"work_package":{"version":1,"summary":"Review"},"vendor_assessment":{"criteria":[{"id":"identity","status":"unknown","reason":"Missing","evidence_ids":[]},{"id":"identity","status":"unknown","reason":"Duplicate","evidence_ids":[]}]}}`
				case "question mixed with assessment":
					content = `{"follow_up_question":"Which vendor?","vendor_assessment":{"criteria":[]}}`
				}
				if structured {
					content = `{"result":` + content + `}`
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
				}))
				defer server.Close()
				p, err := NewOpenAICompatible(server.URL, "", "test", server.Client(), Options{StructuredOutput: structured})
				if err != nil {
					t.Fatal(err)
				}
				result, err := p.Diagnose(context.Background(), request)
				if scenario == "valid" {
					if err != nil || result.VendorAssessment == nil || result.WorkPackage.RiskAssessment != nil {
						t.Fatalf("result=%+v err=%v", result, err)
					}
				} else if err == nil {
					t.Fatal("unsafe vendor result accepted")
				}
			})
		}
	}
}
