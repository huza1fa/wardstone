package agent

import (
	"encoding/json"
	"github.com/wardstone-project/wardstone/internal/domain"
)

// ConstraintForEvidence reads only the current run's connector-owned selection
// observations. Historical gaps must not constrain a repaired configuration.
func ConstraintForEvidence(current []domain.Evidence) string {
	constraint := ""
	for _, item := range current {
		if !((item.Capability == "google.access.inspect" && item.Kind == "google.access_request") || ((item.Capability == "research.vendor.collect" || item.Capability == "research.knowledge.collect") && item.Kind == "research.request_context")) {
			continue
		}
		var context struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(item.Data, &context) != nil {
			continue
		}
		if context.Status == "operator_intervention_required" {
			return "final_report"
		}
		// This bounded research slice answers from the selected documents.
		// Business scope and unavailable artifacts become report unknowns, rather
		// than another question after the source selector is already resolved.
		if item.Kind == "research.request_context" && context.Status == "ready" {
			return "final_report"
		}
		if context.Status == "clarification_required" {
			constraint = "requester_question"
		}
	}
	return constraint
}
