package investigations

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

// FinalizeReport applies the same citation, narrative, and deterministic scoring
// boundary to live investigations and saved-case evaluations. Inputs must be
// frozen before invoking a provider; it cannot rewrite this authority.
func FinalizeReport(specialist domain.SpecialistName, scoringContext domain.VendorReviewContext, result agent.Result, supplied, current []domain.Evidence) (agent.Result, error) {
	if result.WorkPackage != nil && result.WorkPackage.RiskAssessment != nil {
		return agent.Result{}, errors.New("model-supplied overall risk assessment is not accepted")
	}
	if specialist == specialists.VendorReview {
		if result.WorkPackage == nil || result.VendorAssessment == nil {
			return agent.Result{}, errors.New("vendor review requires a report and criterion assessments")
		}
		if err := domain.ValidateVendorNarrative(result.WorkPackage, result.Diagnosis, *result.VendorAssessment); err != nil {
			return agent.Result{}, err
		}
		risk, err := domain.ComputeVendorRisk(scoringContext, *result.VendorAssessment, supplied, current)
		if err != nil {
			return agent.Result{}, fmt.Errorf("compute vendor risk: %w", err)
		}
		result.WorkPackage = result.WorkPackage.Clone()
		result.WorkPackage.RiskAssessment = risk
		// Vendor documents support attributed claims, not independently verified controls.
		claimIDs := map[domain.EvidenceID]bool{}
		for _, e := range supplied {
			var d struct {
				ClaimType string `json:"claim_type"`
			}
			if e.Kind == "research.vendor_document" && json.Unmarshal(e.Data, &d) == nil && d.ClaimType == "vendor_claim" {
				claimIDs[e.ID] = true
			}
		}
		for i, f := range result.WorkPackage.Findings {
			if f.Kind == "verified" {
				for _, id := range f.EvidenceIDs {
					if claimIDs[id] {
						result.WorkPackage.Findings[i].Kind = "vendor_claim"
						break
					}
				}
			}
		}
		result.WorkPackage.RequesterSummary = fmt.Sprintf("Provisional vendor risk: %s. %s %d evidence gap(s) require review. No vendor approval or infrastructure change has been performed.", risk.Rating, risk.Rationale, len(risk.Scoring.Gaps))
		if len(risk.Scoring.Gaps) > 0 {
			result.WorkPackage.EscalationReason = "An operator must resolve the vendor evidence gaps and deployment context before a complete rating is available."
		}
	} else if result.VendorAssessment != nil {
		return agent.Result{}, errors.New("vendor assessment is restricted to the vendor specialist")
	}
	result.WorkPackage = result.WorkPackage.Clone()
	if result.WorkPackage != nil {
		if err := result.WorkPackage.Validate(supplied); err != nil {
			return agent.Result{}, fmt.Errorf("validate work package: %w", err)
		}
		if result.Diagnosis == "" {
			result.Diagnosis = result.WorkPackage.Summary
		}
	}

	if strings.TrimSpace(result.Diagnosis) == "" {
		return agent.Result{}, errors.New("model returned an empty diagnosis")
	}
	if len(result.Diagnosis) > maxDiagnosisSize || !utf8.ValidString(result.Diagnosis) || strings.ContainsRune(result.Diagnosis, 0) {
		return agent.Result{}, fmt.Errorf("model diagnosis exceeds %d bytes", maxDiagnosisSize)
	}

	return result, nil
}
