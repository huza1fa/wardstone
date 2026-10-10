package agent

import (
	"encoding/json"
	"github.com/wardstone-project/wardstone/internal/domain"
	"testing"
)

func TestCurrentSelectorConstraints(t *testing.T) {
	evidence := func(capability domain.CapabilityName, kind, status string) domain.Evidence {
		return domain.Evidence{Capability: capability, Kind: kind, Data: json.RawMessage(`{"status":"` + status + `"}`)}
	}
	ready := evidence("google.access.inspect", "google.access_request", "ready")
	clarify := evidence("research.vendor.collect", "research.request_context", "clarification_required")
	blocked := evidence("google.access.inspect", "google.access_request", "operator_intervention_required")
	if ConstraintForEvidence([]domain.Evidence{ready}) != "" {
		t.Fatal("ready evidence constrained the response")
	}
	if ConstraintForEvidence([]domain.Evidence{clarify}) != "requester_question" {
		t.Fatal("missing selector was not clarified")
	}
	if ConstraintForEvidence([]domain.Evidence{clarify, blocked}) != "final_report" {
		t.Fatal("operator gap did not take precedence")
	}
	forged := evidence("research.vendor.collect", "research.vendor_document", "operator_intervention_required")
	if ConstraintForEvidence([]domain.Evidence{forged}) != "" {
		t.Fatal("vendor document content changed runtime workflow")
	}
}
