// Package evaluations runs bounded saved final-report cases through a provider.
// It has no store, collectors, delivery dispatcher, or execution adapter.
package evaluations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

const MaxDatasetBytes = 16 << 20
const MaxCases = 100

type Expectation struct {
	Rating              string              `json:"rating,omitempty"`
	RequiredEvidenceIDs []domain.EvidenceID `json:"required_evidence_ids,omitempty"`
	ForbiddenPhrases    []string            `json:"forbidden_phrases,omitempty"`
	RequireUnknowns     bool                `json:"require_unknowns,omitempty"`
}
type Case struct {
	Name            string            `json:"name"`
	Request         agent.Request     `json:"request"`
	CurrentEvidence []domain.Evidence `json:"current_evidence"`
	RecordedResult  *agent.Result     `json:"recorded_result,omitempty"`
	Expect          Expectation       `json:"expect"`
}
type Dataset struct {
	Version int    `json:"version"`
	Cases   []Case `json:"cases"`
}

func Read(r io.Reader) (Dataset, error) {
	var d Dataset
	raw, err := io.ReadAll(io.LimitReader(r, MaxDatasetBytes+1))
	if err != nil || len(raw) > MaxDatasetBytes {
		return d, errors.New("evaluation dataset exceeds 16 MiB or could not be read")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&d); err != nil {
		return d, fmt.Errorf("decode evaluation dataset: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return d, errors.New("evaluation dataset must be one JSON object")
	}
	if d.Version != 1 || len(d.Cases) == 0 || len(d.Cases) > MaxCases {
		return d, errors.New("dataset version 1 and 1–100 cases are required")
	}
	names, ids := map[string]bool{}, map[domain.InvestigationID]bool{}
	for _, c := range d.Cases {
		if strings.TrimSpace(c.Name) == "" || len(c.Name) > 128 || names[c.Name] || c.Request.InvestigationID == "" || ids[c.Request.InvestigationID] {
			return d, errors.New("case names and investigation IDs must be nonempty and unique")
		}
		names[c.Name] = true
		ids[c.Request.InvestigationID] = true
		if c.Request.Ticket.Validate() != nil {
			return d, fmt.Errorf("case %q has an invalid ticket", c.Name)
		}
		raw, _ := json.Marshal(c.Request)
		if len(raw) > agent.MaxDiagnosisPromptBytes-8192 || len(c.Request.Evidence) > 128 || len(c.CurrentEvidence) > 256 {
			return d, errors.New("saved request exceeds investigation evidence/prompt bounds")
		}
		seen := map[domain.EvidenceID]bool{}
		for _, e := range c.Request.Evidence {
			if e.ID == "" || seen[e.ID] || !json.Valid(e.Data) {
				return d, errors.New("saved evidence requires unique IDs and JSON data")
			}
			seen[e.ID] = true
		}
		if len(c.Expect.ForbiddenPhrases) > 32 || len(c.Expect.RequiredEvidenceIDs) > 128 {
			return d, errors.New("evaluation expectations exceed bounds")
		}
		for _, phrase := range c.Expect.ForbiddenPhrases {
			if strings.TrimSpace(phrase) == "" || len(phrase) > 256 {
				return d, errors.New("forbidden phrases must contain 1–256 bytes")
			}
		}
		for _, id := range c.Expect.RequiredEvidenceIDs {
			if !seen[id] {
				return d, errors.New("required citation is missing from saved evidence")
			}
		}
		if c.Expect.Rating != "" && c.Expect.Rating != "LOW" && c.Expect.Rating != "MODERATE" && c.Expect.Rating != "HIGH" && c.Expect.Rating != "UNKNOWN" {
			return d, errors.New("unknown expected risk rating")
		}
		if c.Request.Specialist == specialists.VendorReview {
			if c.Request.VendorReview == nil {
				return d, errors.New("vendor evaluation requires administrator scoring context")
			}
			if c.Request.VendorReview.Rubric != nil && c.Request.VendorReview.Rubric.Validate() != nil {
				return d, errors.New("invalid saved vendor rubric")
			}
		}
	}
	return d, nil
}

type Result struct {
	Case                     string                   `json:"case"`
	Model                    string                   `json:"model"`
	Provider                 domain.ModelProviderName `json:"provider"`
	DurationMS               int64                    `json:"duration_ms"`
	Passed                   bool                     `json:"passed"`
	CitationValid            bool                     `json:"citation_valid"`
	UniqueCitations          int                      `json:"unique_citations"`
	EvidenceComplete         bool                     `json:"evidence_complete"`
	MissingEvidenceDisclosed bool                     `json:"missing_evidence_disclosed"`
	WordingValid             bool                     `json:"wording_valid"`
	ReportValid              bool                     `json:"report_valid"`
	Rating                   string                   `json:"rating,omitempty"`
	Score                    *int                     `json:"score,omitempty"`
	GapCodes                 []string                 `json:"gap_codes"`
	Failures                 []string                 `json:"failures"`
	Response                 *agent.Result            `json:"response,omitempty"`
}
type Report struct {
	Version       int      `json:"version"`
	DatasetSHA256 string   `json:"dataset_sha256"`
	Results       []Result `json:"results"`
	Passed        bool     `json:"passed"`
}

func Run(ctx context.Context, c Case, provider agent.ModelProvider, timeout time.Duration, includeResponse bool) Result {
	out := Result{Case: c.Name, Model: provider.Model(), Provider: provider.Name(), CitationValid: true, EvidenceComplete: true, WordingValid: true, GapCodes: []string{}, Failures: []string{}}
	// Freeze both evidence authority and provider-owned request references.
	raw, _ := json.Marshal(c)
	var frozen Case
	_ = json.Unmarshal(raw, &frozen)
	rawRequest, _ := json.Marshal(c.Request)
	var request agent.Request
	_ = json.Unmarshal(rawRequest, &request)
	start := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	result, err := provider.Diagnose(callCtx, request)
	cancel()
	out.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		out.Failures = append(out.Failures, "provider_call_failed")
		out.CitationValid = false
		out.EvidenceComplete = false
		out.WordingValid = false
		return out
	}
	if includeResponse {
		encoded, _ := json.Marshal(result)
		if len(encoded) > 256<<10 {
			out.Failures = append(out.Failures, "response_capture_limit")
		} else {
			var response agent.Result
			_ = json.Unmarshal(encoded, &response)
			out.Response = &response
		}
	}
	supplied := frozen.Request.Evidence
	current := frozen.CurrentEvidence
	if current == nil {
		current = supplied
	}
	known, cited := map[domain.EvidenceID]bool{}, map[domain.EvidenceID]bool{}
	provenance := map[domain.EvidenceID]domain.Evidence{}
	for _, e := range supplied {
		known[e.ID] = true
		provenance[e.ID] = e
	}
	cite := func(ids []domain.EvidenceID) {
		for _, id := range ids {
			cited[id] = true
			if !known[id] {
				out.CitationValid = false
			}
		}
	}
	if result.WorkPackage != nil {
		for _, f := range result.WorkPackage.Findings {
			cite(f.EvidenceIDs)
		}
		if result.WorkPackage.RiskAssessment != nil {
			cite(result.WorkPackage.RiskAssessment.EvidenceIDs)
		}
	}
	if result.VendorAssessment != nil {
		for _, a := range result.VendorAssessment.Criteria {
			cite(a.EvidenceIDs)
			if frozen.Request.Specialist == specialists.VendorReview && frozen.Request.VendorReview != nil {
				for _, id := range a.EvidenceIDs {
					e := provenance[id]
					var doc struct {
						Resource string `json:"resource"`
						Content  string `json:"content"`
						Claim    string `json:"claim_type"`
					}
					_ = json.Unmarshal(e.Data, &doc)
					if e.Source != "research" || e.Capability != "research.vendor.collect" || e.Kind != "research.vendor_document" || doc.Resource != frozen.Request.VendorReview.Vendor || strings.TrimSpace(doc.Content) == "" || (doc.Claim != "vendor_claim" && doc.Claim != "independent_report") {
						out.CitationValid = false
					}
				}
			}
		}
	}
	for _, a := range result.Actions {
		cite(a.EvidenceIDs)
	}
	out.UniqueCitations = len(cited)
	for _, id := range frozen.Expect.RequiredEvidenceIDs {
		if !cited[id] {
			out.Failures = append(out.Failures, "required_citation_missing")
		}
	}
	for _, e := range current {
		var d struct {
			Truncated bool `json:"truncated"`
		}
		_ = json.Unmarshal(e.Data, &d)
		if e.Kind == "research.document_unavailable" || d.Truncated || !known[e.ID] {
			out.EvidenceComplete = false
		}
	}
	if result.WorkPackage != nil {
		out.MissingEvidenceDisclosed = len(result.WorkPackage.Unknowns) > 0 || strings.TrimSpace(result.WorkPackage.EscalationReason) != ""
	}
	narrative, _ := json.Marshal(result)
	for _, phrase := range frozen.Expect.ForbiddenPhrases {
		if strings.Contains(strings.ToLower(string(narrative)), strings.ToLower(phrase)) {
			out.WordingValid = false
			out.Failures = append(out.Failures, "forbidden_phrase")
		}
	}
	if frozen.Request.Specialist == specialists.VendorReview && result.WorkPackage != nil && result.VendorAssessment != nil {
		if domain.ValidateVendorNarrative(result.WorkPackage, result.Diagnosis, *result.VendorAssessment) != nil {
			out.WordingValid = false
			out.Failures = append(out.Failures, "vendor_narrative_invalid")
		}
	}
	if result.FollowUpQuestion != "" || result.Handoff != nil {
		out.Failures = append(out.Failures, "expected_final_report")
		return out
	}
	var scoring domain.VendorReviewContext
	if frozen.Request.VendorReview != nil {
		scoring = frozen.Request.VendorReview.Clone()
	}
	normalized, err := investigations.FinalizeReport(frozen.Request.Specialist, scoring, result, supplied, current)
	out.ReportValid = err == nil && normalized.WorkPackage != nil && strings.TrimSpace(normalized.Diagnosis) != "" && len(result.Actions) == 0
	if !out.ReportValid {
		out.Failures = append(out.Failures, "report_boundary_invalid")
	}
	if normalized.WorkPackage != nil && normalized.WorkPackage.RiskAssessment != nil {
		risk := normalized.WorkPackage.RiskAssessment
		out.Rating = risk.Rating
		if risk.Scoring != nil {
			out.Score = risk.Scoring.Score
			for _, g := range risk.Scoring.Gaps {
				out.GapCodes = append(out.GapCodes, g.Code)
			}
			if len(risk.Scoring.Gaps) > 0 {
				out.EvidenceComplete = false
			}
		}
	}
	if frozen.Expect.Rating != "" && out.Rating != frozen.Expect.Rating {
		out.Failures = append(out.Failures, "unexpected_rating")
	}
	if frozen.Expect.RequireUnknowns && !out.MissingEvidenceDisclosed {
		out.Failures = append(out.Failures, "missing_evidence_not_disclosed")
	}
	if !out.CitationValid {
		out.Failures = append(out.Failures, "invalid_citation")
	}
	out.Passed = out.ReportValid && out.CitationValid && out.WordingValid && len(out.Failures) == 0
	return out
}

// RecordedProvider exercises the real checks offline. Each saved result is an
// input fixture, not a provider quality measurement or a claim of live testing.
type RecordedProvider struct {
	Results map[domain.InvestigationID]agent.Result
}

func (RecordedProvider) Name() domain.ModelProviderName { return "recorded-fixture" }
func (RecordedProvider) Model() string                  { return "fixture" }
func (p RecordedProvider) Diagnose(_ context.Context, r agent.Request) (agent.Result, error) {
	result, ok := p.Results[r.InvestigationID]
	if !ok {
		return agent.Result{}, errors.New("recorded result missing")
	}
	raw, _ := json.Marshal(result)
	var copy agent.Result
	err := json.Unmarshal(raw, &copy)
	return copy, err
}
