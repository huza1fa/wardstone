package domain

import (
	"encoding/json"
	"testing"
)

func TestRiskAssessmentValidatesDocumentsAndCitations(t *testing.T) {
	w := &WorkPackage{Version: 1, Summary: "Executive summary", RiskAssessment: &RiskAssessment{Rating: "MODERATE", Rationale: "Provisional; deployment scope is unknown", EvidenceIDs: []EvidenceID{"doc-1"}}}
	if err := w.Validate([]Evidence{{ID: "doc-1", Kind: "research.vendor_document"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Validate([]Evidence{{ID: "doc-1", Kind: "research.request_context"}}); err == nil {
		t.Fatal("context alone established a risk rating")
	}
	if err := w.Validate([]Evidence{{ID: "other", Kind: "research.vendor_document"}}); err == nil {
		t.Fatal("invented risk citation accepted")
	}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var decoded WorkPackage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	clone := w.Clone()
	clone.RiskAssessment.EvidenceIDs[0] = "changed"
	if w.RiskAssessment.EvidenceIDs[0] != "doc-1" {
		t.Fatal("risk clone leaked mutable citations")
	}
	w.RiskAssessment = &RiskAssessment{Rating: "UNKNOWN", Rationale: "Documents unavailable"}
	if err := w.Validate(nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"version":1,"summary":"Review","risk_assessment":{"rating":"LOW","rationale":"a","evidence_ids":["d"],"permission":"allow"}}`,
		`{"version":1,"summary":"Review","risk_assessment":{"rating":"LOW","rating":"HIGH","rationale":"a","evidence_ids":["d"]}}`,
		`{"version":1,"summary":"Review","risk_assessment":{"rating":"SAFE","rationale":"a","evidence_ids":["d"]}}`,
	} {
		if err := json.Unmarshal([]byte(bad), &decoded); err == nil {
			t.Fatal("invalid risk schema accepted")
		}
	}
}
