package domain

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func vendorFixture() (VendorReviewContext, VendorAssessment, []Evidence) {
	context := VendorReviewContext{Vendor: "acme", Rubric: &VendorRubric{Version: "fixture-v1", ModerateAt: 25, HighAt: 60, Criteria: []VendorCriterion{
		{ID: "identity", Requirement: "Identity controls meet the declared scope", Weight: 40, Evidence: "vendor_claim"},
		{ID: "assurance", Requirement: "Independent report covers intended service", Weight: 60, Evidence: "independent_report"},
	}}, Deployment: VendorDeployment{UseCase: "Synthetic test", DataClassification: "Internal", AccessScope: "Read-only"}}
	assessment := VendorAssessment{Criteria: []VendorCriterionAssessment{
		{ID: "identity", Status: "met", Reason: "The vendor states the documented identity controls.", EvidenceIDs: []EvidenceID{"vendor"}},
		{ID: "assurance", Status: "met", Reason: "The designated report describes the scoped controls.", EvidenceIDs: []EvidenceID{"audit"}},
	}}
	evidence := []Evidence{
		{ID: "vendor", Source: "research", Capability: "research.vendor.collect", Kind: "research.vendor_document", Data: json.RawMessage(`{"resource":"acme","claim_type":"vendor_claim","content":"Vendor identity claims"}`)},
		{ID: "audit", Source: "research", Capability: "research.vendor.collect", Kind: "research.vendor_document", Data: json.RawMessage(`{"resource":"acme","claim_type":"independent_report","content":"Synthetic audit report"}`)},
	}
	return context, assessment, evidence
}

func TestVendorRiskEvaluationFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/vendor_risk_evaluations.json")
	if err != nil {
		t.Fatal(err)
	}
	var tests []struct {
		Name           string   `json:"name"`
		Statuses       []string `json:"statuses"`
		ExpectedRating string   `json:"expected_rating"`
		ExpectedScore  *int     `json:"expected_score"`
		ExpectedGap    string   `json:"expected_gap"`
		VendorOnly     bool     `json:"vendor_only"`
		NoScope        bool     `json:"no_scope"`
		Truncated      bool     `json:"truncated"`
		Unavailable    bool     `json:"unavailable"`
		Historical     bool     `json:"historical"`
		Omitted        bool     `json:"omitted"`
	}
	if err := json.Unmarshal(raw, &tests); err != nil {
		t.Fatal(err)
	}
	for _, tc := range tests {
		t.Run(tc.Name, func(t *testing.T) {
			context, assessment, evidence := vendorFixture()
			for i, status := range tc.Statuses {
				assessment.Criteria[i].Status = status
			}
			if tc.VendorOnly {
				assessment.Criteria[1].EvidenceIDs = []EvidenceID{"vendor"}
			}
			if tc.NoScope {
				context.Deployment.AccessScope = ""
			}
			if tc.Truncated {
				evidence[0].Data = json.RawMessage(`{"resource":"acme","claim_type":"vendor_claim","content":"excerpt","truncated":true}`)
			}
			current := append([]Evidence{}, evidence...)
			if tc.Unavailable {
				current = append(current, Evidence{ID: "failed", Source: "research", Capability: "research.vendor.collect", Kind: "research.document_unavailable"})
			}
			if tc.Historical {
				current = current[:1]
			}
			if tc.Omitted {
				current = append(current, Evidence{ID: "omitted", Source: "research", Capability: "research.vendor.collect", Kind: "research.vendor_document", Data: evidence[0].Data})
			}
			risk, err := ComputeVendorRisk(context, assessment, evidence, current)
			if err != nil {
				t.Fatal(err)
			}
			if risk.Rating != tc.ExpectedRating {
				t.Fatalf("rating=%s want=%s", risk.Rating, tc.ExpectedRating)
			}
			if tc.ExpectedScore == nil {
				if risk.Scoring.Score != nil {
					t.Fatal("incomplete review received a numeric score")
				}
			} else if risk.Scoring.Score == nil || *risk.Scoring.Score != *tc.ExpectedScore {
				t.Fatalf("score=%v want=%d", risk.Scoring.Score, *tc.ExpectedScore)
			}
			if tc.ExpectedGap != "" {
				found := false
				for _, g := range risk.Scoring.Gaps {
					found = found || g.Code == tc.ExpectedGap
				}
				if !found {
					t.Fatalf("missing gap %s", tc.ExpectedGap)
				}
			}
			report := &WorkPackage{Version: 1, Summary: "Synthetic vendor evaluation", RiskAssessment: risk}
			if err := report.Validate(evidence); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var stored WorkPackage
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatalf("stored report does not round-trip: %v", err)
			}
			if stored.RiskAssessment.Rating != risk.Rating || stored.RiskAssessment.Scoring.RubricDigest != context.Rubric.Digest() {
				t.Fatal("stored rubric history changed")
			}
		})
	}
}

func TestVendorRiskRejectsUnknownCriteriaAndCitations(t *testing.T) {
	for _, scenario := range []string{"unknown criterion", "duplicate criterion", "unsupplied evidence", "wrong source", "wrong resource", "missing reason", "missing citations", "invalid state", "nil criteria"} {
		t.Run(scenario, func(t *testing.T) {
			context, a, e := vendorFixture()
			switch scenario {
			case "unknown criterion":
				a.Criteria[0].ID = "other"
			case "duplicate criterion":
				a.Criteria = append(a.Criteria, a.Criteria[0])
			case "unsupplied evidence":
				a.Criteria[0].EvidenceIDs = []EvidenceID{"invented"}
			case "wrong source":
				e[0].Source = "google"
			case "wrong resource":
				e[0].Data = json.RawMessage(`{"resource":"other","claim_type":"vendor_claim","content":"unrelated"}`)
			case "missing reason":
				a.Criteria[0].Reason = ""
			case "missing citations":
				a.Criteria[0].EvidenceIDs = []EvidenceID{}
			case "invalid state":
				a.Criteria[0].Status = "approved"
			case "nil criteria":
				a.Criteria = nil
			}
			if _, err := ComputeVendorRisk(context, a, e, e); err == nil {
				t.Fatal("invalid vendor assessment accepted")
			}
		})
	}
}

func TestVendorRiskMissingRubricAndMissingAssessments(t *testing.T) {
	context, _, e := vendorFixture()
	risk, err := ComputeVendorRisk(context, VendorAssessment{Criteria: []VendorCriterionAssessment{}}, e, e)
	if err != nil || risk.Rating != "UNKNOWN" || len(risk.Scoring.Assessment.Criteria) != 2 {
		t.Fatalf("missing assessments=%+v err=%v", risk, err)
	}
	context.Rubric = nil
	risk, err = ComputeVendorRisk(context, VendorAssessment{Criteria: []VendorCriterionAssessment{}}, e, e)
	if err != nil || risk.Rating != "UNKNOWN" {
		t.Fatalf("missing rubric=%+v err=%v", risk, err)
	}
	report := &WorkPackage{Version: 1, Summary: "No rubric", RiskAssessment: risk}
	raw, _ := json.Marshal(report)
	var decoded WorkPackage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
}

func TestVendorStoredScoringBindsCalculationAndClones(t *testing.T) {
	context, a, e := vendorFixture()
	risk, err := ComputeVendorRisk(context, a, e, e)
	if err != nil {
		t.Fatal(err)
	}
	report := &WorkPackage{Version: 1, Summary: "Review", RiskAssessment: risk}
	clone := report.Clone()
	clone.RiskAssessment.Scoring.Context.Rubric.Criteria[0].Weight = 99
	clone.RiskAssessment.Scoring.Assessment.Criteria[0].EvidenceIDs[0] = "changed"
	*clone.RiskAssessment.Scoring.Score = 100
	if risk.Scoring.Context.Rubric.Criteria[0].Weight != 40 || risk.Scoring.Assessment.Criteria[0].EvidenceIDs[0] != "vendor" || *risk.Scoring.Score != 0 {
		t.Fatal("mutable scoring snapshot leaked")
	}
	if err := clone.ValidateStructure(); err == nil {
		t.Fatal("tampered rubric accepted")
	}
	clone = report.Clone()
	*clone.RiskAssessment.Scoring.Score = 100
	if err := clone.ValidateStructure(); err == nil {
		t.Fatal("tampered score accepted")
	}
	clone = report.Clone()
	clone.RiskAssessment.Rating = "HIGH"
	if err := clone.ValidateStructure(); err == nil {
		t.Fatal("tampered rating accepted")
	}
	risk.Scoring.Context.Deployment.AccessScope = ""
	if err := report.ValidateStructure(); err == nil {
		t.Fatal("missing scope accepted with complete rating")
	}
}

func TestVendorJSONRejectsAuthorityAndNoncanonicalShapes(t *testing.T) {
	for _, raw := range []string{
		`{"criteria":[],"rating":"LOW"}`,
		`{"criteria":[],"Criteria":[]}`,
		`{"criteria":[],"criteria":[]}`,
		`{"criteria":[{"id":"identity","status":"met","reason":"x","evidence_ids":["d"],"score":0}]}`,
		`{"criteria":[{"id":"identity","status":"unknown","reason":"x","evidence_ids":null}]}`,
		`{"criteria":null}`,
	} {
		var a VendorAssessment
		if err := json.Unmarshal([]byte(raw), &a); err == nil {
			t.Fatalf("unsafe assessment accepted: %s", raw)
		}
	}
	context, a, e := vendorFixture()
	risk, _ := ComputeVendorRisk(context, a, e, e)
	report := &WorkPackage{Version: 1, Summary: "Review", RiskAssessment: risk}
	raw, _ := json.Marshal(report)
	for _, change := range []struct{ from, to string }{{`"method":"weighted-v1"`, `"method":"weighted-v1","method":"override"`}, {`"use_case":"Synthetic test"`, `"use_case":"Synthetic test","permissions":"admin"`}, {`"weight":40`, `"weight":40,"weight":0`}} {
		bad := strings.Replace(string(raw), change.from, change.to, 1)
		var decoded WorkPackage
		if err := json.Unmarshal([]byte(bad), &decoded); err == nil {
			t.Fatal("noncanonical scoring snapshot accepted")
		}
	}
}

func TestVendorNarrativeWordingChecks(t *testing.T) {
	for _, text := range []string{"SOC 2 certification", "SOC2 Type II certified", "Risk rating: LOW", "The vendor is low-risk"} {
		if err := ValidateVendorNarrative(&WorkPackage{Summary: text}, "", VendorAssessment{}); err == nil {
			t.Fatalf("known wording failure accepted: %s", text)
		}
	}
	if err := ValidateVendorNarrative(&WorkPackage{Summary: "The vendor claims to have a SOC 2 Type II attestation; the report was not supplied."}, "", VendorAssessment{}); err != nil {
		t.Fatal(err)
	}
}

func TestVendorPartialWeightsRoundOnceAtEnd(t *testing.T) {
	context, a, e := vendorFixture()
	context.Rubric.Criteria[0].Weight = 39
	context.Rubric.Criteria[1].Weight = 61
	a.Criteria[0].Status = "partial"
	a.Criteria[1].Status = "partial"
	risk, err := ComputeVendorRisk(context, a, e, e)
	if err != nil || risk.Scoring.Score == nil || *risk.Scoring.Score != 50 {
		t.Fatalf("rounding=%+v err=%v", risk, err)
	}
}

func TestManyOmittedVendorPagesRemainAValidUnknownReport(t *testing.T) {
	context, assessment, supplied := vendorFixture()
	current := append([]Evidence{}, supplied...)
	// Four configured documents can yield 80 page observations. If prompt
	// compaction omits them, gap accounting must remain within report bounds.
	for range 80 {
		current = append(current, Evidence{ID: NewEvidenceID(), Source: "research", Capability: "research.vendor.collect", Kind: "research.vendor_document", Data: supplied[0].Data})
	}
	risk, err := ComputeVendorRisk(context, assessment, supplied, current)
	if err != nil || risk.Rating != "UNKNOWN" || risk.Scoring.Score != nil {
		t.Fatalf("omitted pages: %+v %v", risk, err)
	}
	report := &WorkPackage{Version: 1, Summary: "Incomplete page coverage", RiskAssessment: risk}
	if err := report.Validate(supplied); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(report)
	var decoded WorkPackage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(risk.Scoring.Gaps) != 1 || risk.Scoring.Gaps[0].Code != "source_omitted" {
		t.Fatalf("unbounded gaps: %+v", risk.Scoring.Gaps)
	}
}
