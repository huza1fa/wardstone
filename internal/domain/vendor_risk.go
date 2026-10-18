package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const MaxVendorCriteria = 8

// VendorRiskConfig is administrator-owned policy, never ticket/model authority.
type VendorRiskConfig struct {
	Rubric      *VendorRubric               `yaml:"rubric"`
	Deployments map[string]VendorDeployment `yaml:"deployments"`
}

type VendorRubric struct {
	Version    string            `json:"version" yaml:"version"`
	ModerateAt int               `json:"moderate_at" yaml:"moderate_at"`
	HighAt     int               `json:"high_at" yaml:"high_at"`
	Criteria   []VendorCriterion `json:"criteria" yaml:"criteria"`
}

type VendorCriterion struct {
	ID          string `json:"id" yaml:"id"`
	Requirement string `json:"requirement" yaml:"requirement"`
	Weight      int    `json:"weight" yaml:"weight"`
	Evidence    string `json:"evidence" yaml:"evidence"` // vendor_claim or independent_report
}

type VendorDeployment struct {
	UseCase            string `json:"use_case" yaml:"use_case"`
	DataClassification string `json:"data_classification" yaml:"data_classification"`
	AccessScope        string `json:"access_scope" yaml:"access_scope"`
}

type VendorReviewContext struct {
	Vendor     string           `json:"vendor"`
	Rubric     *VendorRubric    `json:"rubric,omitempty"`
	Deployment VendorDeployment `json:"deployment"`
}

// VendorAssessment is model interpretation. It has no overall rating or score.
type VendorAssessment struct {
	Criteria []VendorCriterionAssessment `json:"criteria"`
}

type VendorCriterionAssessment struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"` // met, partial, unmet, unknown
	Reason      string       `json:"reason"`
	EvidenceIDs []EvidenceID `json:"evidence_ids"`
}

type VendorEvidenceGap struct {
	Code        string       `json:"code"`
	Detail      string       `json:"detail"`
	EvidenceIDs []EvidenceID `json:"evidence_ids"`
}

// VendorScoring is produced by Go and stored with the immutable report.
type VendorScoring struct {
	Method       string              `json:"method"`
	Context      VendorReviewContext `json:"context"`
	RubricDigest string              `json:"rubric_digest"`
	Assessment   VendorAssessment    `json:"assessment"`
	Gaps         []VendorEvidenceGap `json:"gaps"`
	Score        *int                `json:"score,omitempty"`
}

var criterionID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

func (r VendorRubric) Validate() error {
	if validateText("rubric version", r.Version, 64, true) != nil || strings.TrimSpace(r.Version) != r.Version || r.ModerateAt < 1 || r.HighAt <= r.ModerateAt || r.HighAt > 100 || len(r.Criteria) < 1 || len(r.Criteria) > MaxVendorCriteria {
		return errors.New("invalid vendor rubric version, thresholds, or criteria count")
	}
	total := 0
	seen := map[string]bool{}
	for _, c := range r.Criteria {
		if !criterionID.MatchString(c.ID) || seen[c.ID] || c.Weight < 1 || c.Weight > 100 || validateText("criterion requirement", c.Requirement, 600, true) != nil || (c.Evidence != "vendor_claim" && c.Evidence != "independent_report") {
			return errors.New("invalid or duplicate vendor criterion")
		}
		seen[c.ID] = true
		total += c.Weight
	}
	if total != 100 {
		return errors.New("vendor criterion weights must sum to 100")
	}
	return nil
}

func (r *VendorRubric) Clone() *VendorRubric {
	if r == nil {
		return nil
	}
	copy := *r
	copy.Criteria = append([]VendorCriterion(nil), r.Criteria...)
	return &copy
}
func (r *VendorRubric) Digest() string {
	if r == nil {
		return ""
	}
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (c VendorRiskConfig) Validated() (VendorRiskConfig, error) {
	copy := VendorRiskConfig{Rubric: c.Rubric.Clone(), Deployments: map[string]VendorDeployment{}}
	if copy.Rubric != nil {
		if err := copy.Rubric.Validate(); err != nil {
			return copy, err
		}
	}
	if len(c.Deployments) > 128 {
		return copy, errors.New("vendor deployments exceed 128 entries")
	}
	for key, deployment := range c.Deployments {
		name := strings.ToLower(strings.TrimSpace(key))
		if validateText("vendor deployment name", name, 128, true) != nil {
			return copy, errors.New("invalid vendor deployment name")
		}
		if _, exists := copy.Deployments[name]; exists {
			return copy, errors.New("duplicate normalized vendor deployment")
		}
		for _, value := range []string{deployment.UseCase, deployment.DataClassification, deployment.AccessScope} {
			if validateText("vendor deployment context", value, 600, false) != nil {
				return copy, errors.New("invalid vendor deployment context")
			}
		}
		copy.Deployments[name] = deployment
	}
	return copy, nil
}

func (c VendorReviewContext) Clone() VendorReviewContext { c.Rubric = c.Rubric.Clone(); return c }

func (a VendorAssessment) ValidateStructure() error {
	if a.Criteria == nil || len(a.Criteria) > MaxVendorCriteria {
		return errors.New("vendor assessment requires a bounded criteria array")
	}
	seen := map[string]bool{}
	for _, c := range a.Criteria {
		if !criterionID.MatchString(c.ID) || seen[c.ID] || (c.Status != "met" && c.Status != "partial" && c.Status != "unmet" && c.Status != "unknown") || validateText("assessment reason", c.Reason, 600, true) != nil || c.EvidenceIDs == nil || len(c.EvidenceIDs) > 4 || (c.Status != "unknown" && len(c.EvidenceIDs) == 0) {
			return errors.New("invalid or duplicate vendor criterion assessment")
		}
		seen[c.ID] = true
		ids := map[EvidenceID]bool{}
		for _, id := range c.EvidenceIDs {
			if validateText("vendor evidence ID", string(id), 128, true) != nil || strings.TrimSpace(string(id)) != string(id) || ids[id] {
				return errors.New("invalid or duplicate vendor citation")
			}
			ids[id] = true
		}
	}
	return nil
}

// ComputeVendorRisk treats any incomplete input as UNKNOWN. The caller supplies
// only observations shown to the model, plus IDs from this collection run.
func ComputeVendorRisk(context VendorReviewContext, assessment VendorAssessment, supplied, current []Evidence) (*RiskAssessment, error) {
	if err := assessment.ValidateStructure(); err != nil {
		return nil, err
	}
	context = context.Clone()
	if context.Rubric != nil {
		if err := context.Rubric.Validate(); err != nil {
			return nil, err
		}
	}
	scoring := &VendorScoring{Method: "weighted-v1", Context: context, RubricDigest: context.Rubric.Digest(), Assessment: VendorAssessment{Criteria: []VendorCriterionAssessment{}}, Gaps: []VendorEvidenceGap{}}
	gap := func(code, detail string, ids ...EvidenceID) {
		if ids == nil {
			ids = []EvidenceID{}
		}
		scoring.Gaps = append(scoring.Gaps, VendorEvidenceGap{Code: code, Detail: detail, EvidenceIDs: ids})
	}
	if context.Rubric == nil {
		gap("rubric_unconfigured", "An operator must configure a vendor risk rubric.")
	}
	if context.Vendor == "" {
		gap("vendor_unresolved", "A current-run vendor selector is unavailable.")
	}
	for _, field := range []struct{ name, value string }{{"use_case", context.Deployment.UseCase}, {"data_classification", context.Deployment.DataClassification}, {"access_scope", context.Deployment.AccessScope}} {
		if strings.TrimSpace(field.value) == "" {
			gap("deployment_context_missing", "Administrator deployment context is missing: "+field.name)
		}
	}
	known := map[EvidenceID]Evidence{}
	currentIDs := map[EvidenceID]bool{}
	for _, e := range supplied {
		known[e.ID] = e
	}
	for _, e := range current {
		currentIDs[e.ID] = true
	}
	type document struct {
		ClaimType string `json:"claim_type"`
		Truncated bool   `json:"truncated"`
		Resource  string `json:"resource"`
		Content   string `json:"content"`
	}
	documents := map[EvidenceID]document{}
	for _, e := range supplied {
		if e.Source != "research" || e.Capability != "research.vendor.collect" || e.Kind != "research.vendor_document" {
			continue
		}
		var d document
		if json.Unmarshal(e.Data, &d) != nil || d.Resource != context.Vendor || strings.TrimSpace(d.Content) == "" || (d.ClaimType != "vendor_claim" && d.ClaimType != "independent_report") {
			continue
		}
		documents[e.ID] = d
	}
	usable := 0
	for _, e := range current {
		if e.Source != "research" || e.Capability != "research.vendor.collect" {
			continue
		}
		if e.Kind == "research.document_unavailable" {
			gap("source_unavailable", "A configured vendor source could not be collected.")
			continue
		}
		if e.Kind != "research.vendor_document" {
			continue
		}
		d, ok := documents[e.ID]
		if !ok {
			gap("source_omitted", "A current vendor source is missing or unusable in model context.")
			continue
		}
		usable++
		if d.Truncated {
			gap("source_truncated", "A vendor source exceeded the excerpt budget.", e.ID)
		}
	}
	if usable == 0 {
		gap("no_current_documents", "No usable current-run vendor documents were supplied.")
	}
	byID := map[string]VendorCriterionAssessment{}
	allowed := map[string]bool{}
	if context.Rubric != nil {
		for _, criterion := range context.Rubric.Criteria {
			allowed[criterion.ID] = true
		}
	}
	for _, c := range assessment.Criteria {
		if !allowed[c.ID] {
			return nil, fmt.Errorf("assessment references unknown criterion %q", c.ID)
		}
		c.EvidenceIDs = append([]EvidenceID{}, c.EvidenceIDs...)
		for _, id := range c.EvidenceIDs {
			if _, ok := known[id]; !ok {
				return nil, errors.New("vendor assessment cites unsupplied evidence")
			}
			if _, ok := documents[id]; !ok {
				return nil, errors.New("vendor assessment must cite matching vendor documents with explicit provenance")
			}
		}
		byID[c.ID] = c
	}
	if context.Rubric != nil {
		for _, criterion := range context.Rubric.Criteria {
			c, ok := byID[criterion.ID]
			if !ok {
				c = VendorCriterionAssessment{ID: criterion.ID, Status: "unknown", Reason: "The model did not assess this criterion.", EvidenceIDs: []EvidenceID{}}
			}
			independent := false
			historical := false
			for _, id := range c.EvidenceIDs {
				independent = independent || documents[id].ClaimType == "independent_report"
				historical = historical || !currentIDs[id]
			}
			if historical {
				gap("historical_evidence", "Criterion "+c.ID+" cites observations from a previous run.", c.EvidenceIDs...)
			}
			if c.Status == "unknown" {
				gap("criterion_unknown", "Criterion "+c.ID+" has insufficient or conflicting evidence.", c.EvidenceIDs...)
			}
			if c.Status != "unknown" && criterion.Evidence == "independent_report" && !independent {
				gap("independent_report_missing", "Criterion "+c.ID+" requires an administrator-designated independent report.", c.EvidenceIDs...)
			}
			scoring.Assessment.Criteria = append(scoring.Assessment.Criteria, c)
		}
	}
	risk := &RiskAssessment{Rating: "UNKNOWN", Rationale: "Rating withheld: required context or evidence is incomplete. Review the evidence gaps.", EvidenceIDs: assessmentEvidenceIDs(scoring.Assessment), Scoring: scoring}
	if len(scoring.Gaps) == 0 {
		score, rating := scoreVendorCriteria(*context.Rubric, scoring.Assessment)
		scoring.Score = &score
		risk.Rating = rating
		risk.Rationale = fmt.Sprintf("Go computed %s from %d/100 risk points using rubric %s (MODERATE at %d, HIGH at %d). Criterion interpretations and source claims require operator review.", rating, score, context.Rubric.Version, context.Rubric.ModerateAt, context.Rubric.HighAt)
	}
	return risk, nil
}

func scoreVendorCriteria(r VendorRubric, a VendorAssessment) (int, string) {
	statuses := map[string]string{}
	for _, c := range a.Criteria {
		statuses[c.ID] = c.Status
	}
	twice := 0
	for _, c := range r.Criteria {
		if statuses[c.ID] == "unmet" {
			twice += 2 * c.Weight
		} else if statuses[c.ID] == "partial" {
			twice += c.Weight
		}
	}
	score := (twice + 1) / 2
	rating := "LOW"
	if score >= r.HighAt {
		rating = "HIGH"
	} else if score >= r.ModerateAt {
		rating = "MODERATE"
	}
	return score, rating
}

func assessmentEvidenceIDs(a VendorAssessment) []EvidenceID {
	ids := map[EvidenceID]bool{}
	for _, c := range a.Criteria {
		for _, id := range c.EvidenceIDs {
			ids[id] = true
		}
	}
	result := make([]EvidenceID, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func (s *VendorScoring) Validate(risk *RiskAssessment) error {
	for _, field := range []struct {
		value string
		limit int
	}{{s.Context.Vendor, 128}, {s.Context.Deployment.UseCase, 600}, {s.Context.Deployment.DataClassification, 600}, {s.Context.Deployment.AccessScope, 600}} {
		if validateText("stored vendor context", field.value, field.limit, false) != nil {
			return errors.New("invalid stored vendor context")
		}
	}
	if s.Method != "weighted-v1" || s.Context.Rubric.Digest() != s.RubricDigest || len(s.Gaps) > 32 {
		return errors.New("invalid vendor scoring metadata")
	}
	if err := s.Assessment.ValidateStructure(); err != nil {
		return err
	}
	ids := assessmentEvidenceIDs(s.Assessment)
	if len(ids) != len(risk.EvidenceIDs) {
		return errors.New("vendor scoring citations do not match assessment")
	}
	for i, id := range ids {
		if risk.EvidenceIDs[i] != id {
			return errors.New("vendor scoring citations are not canonical")
		}
	}

	if s.Context.Rubric != nil {
		if err := s.Context.Rubric.Validate(); err != nil {
			return err
		}
	}
	expectedIDs := map[string]bool{}
	if s.Context.Rubric != nil {
		for _, c := range s.Context.Rubric.Criteria {
			expectedIDs[c.ID] = true
		}
	}
	for _, c := range s.Assessment.Criteria {
		if !expectedIDs[c.ID] {
			return errors.New("stored assessment references unknown criterion")
		}
	}
	for _, g := range s.Gaps {
		for _, id := range g.EvidenceIDs {
			if validateText("gap citation", string(id), 128, true) != nil {
				return errors.New("invalid gap citation")
			}
		}
		if !criterionID.MatchString(g.Code) || validateText("evidence gap", g.Detail, 600, true) != nil || len(g.EvidenceIDs) > 4 {
			return errors.New("invalid vendor evidence gap")
		}
	}
	if len(s.Gaps) > 0 {
		if risk.Rating != "UNKNOWN" || s.Score != nil {
			return errors.New("incomplete vendor review cannot have a score")
		}
		return nil
	}
	if s.Context.Rubric == nil || s.Context.Vendor == "" || strings.TrimSpace(s.Context.Deployment.UseCase) == "" || strings.TrimSpace(s.Context.Deployment.DataClassification) == "" || strings.TrimSpace(s.Context.Deployment.AccessScope) == "" || len(s.Assessment.Criteria) != len(s.Context.Rubric.Criteria) {
		return errors.New("complete vendor score requires rubric, context, and all criteria")
	}
	expected := map[string]bool{}
	for _, c := range s.Context.Rubric.Criteria {
		expected[c.ID] = true
	}
	for _, c := range s.Assessment.Criteria {
		if !expected[c.ID] || c.Status == "unknown" {
			return errors.New("incomplete vendor criteria")
		}
	}
	score, rating := scoreVendorCriteria(*s.Context.Rubric, s.Assessment)
	if s.Score == nil || *s.Score != score || risk.Rating != rating {
		return errors.New("vendor rating does not match deterministic score")
	}
	return nil
}

func (a *VendorAssessment) UnmarshalJSON(data []byte) error {
	fields, err := strictFields(data, "criteria")
	if err != nil {
		return err
	}
	var criteria []json.RawMessage
	if err = json.Unmarshal(fields["criteria"], &criteria); err != nil {
		return err
	}
	for _, raw := range criteria {
		if _, err := strictFields(raw, "id", "status", "reason", "evidence_ids"); err != nil {
			return err
		}
	}
	type plain VendorAssessment
	var result plain
	if err = json.Unmarshal(data, &result); err != nil {
		return err
	}
	if err = VendorAssessment(result).ValidateStructure(); err != nil {
		return err
	}
	*a = VendorAssessment(result)
	return nil
}

func (r *VendorRubric) UnmarshalJSON(data []byte) error {
	fields, err := strictFields(data, "version", "moderate_at", "high_at", "criteria")
	if err != nil {
		return err
	}
	var criteria []json.RawMessage
	if err = json.Unmarshal(fields["criteria"], &criteria); err != nil {
		return err
	}
	for _, raw := range criteria {
		if _, err := strictFields(raw, "id", "requirement", "weight", "evidence"); err != nil {
			return err
		}
	}
	type plain VendorRubric
	var result plain
	if err = json.Unmarshal(data, &result); err != nil {
		return err
	}
	if err = VendorRubric(result).Validate(); err != nil {
		return err
	}
	*r = VendorRubric(result)
	return nil
}

func (s *VendorScoring) UnmarshalJSON(data []byte) error {
	fields, err := strictFields(data, "method", "context", "rubric_digest", "assessment", "gaps", "score")
	if err != nil {
		return err
	}
	context, err := strictFields(fields["context"], "vendor", "rubric", "deployment")
	if err != nil {
		return err
	}
	if _, err := strictFields(context["deployment"], "use_case", "data_classification", "access_scope"); err != nil {
		return err
	}
	var gaps []json.RawMessage
	if err = json.Unmarshal(fields["gaps"], &gaps); err != nil {
		return err
	}
	for _, raw := range gaps {
		if _, err := strictFields(raw, "code", "detail", "evidence_ids"); err != nil {
			return err
		}
	}
	type plain VendorScoring
	var result plain
	if err = json.Unmarshal(data, &result); err != nil {
		return err
	}
	*s = VendorScoring(result)
	return nil
}

func (s *VendorScoring) Clone() *VendorScoring {
	if s == nil {
		return nil
	}
	copy := *s
	copy.Context = s.Context.Clone()
	copy.Assessment.Criteria = append([]VendorCriterionAssessment{}, s.Assessment.Criteria...)
	for i := range copy.Assessment.Criteria {
		copy.Assessment.Criteria[i].EvidenceIDs = append([]EvidenceID{}, s.Assessment.Criteria[i].EvidenceIDs...)
	}
	copy.Gaps = append([]VendorEvidenceGap{}, s.Gaps...)
	for i := range copy.Gaps {
		copy.Gaps[i].EvidenceIDs = append([]EvidenceID{}, s.Gaps[i].EvidenceIDs...)
	}
	if s.Score != nil {
		score := *s.Score
		copy.Score = &score
	}
	return &copy
}

var vendorCertificationClaim = regexp.MustCompile(`(?i)\bsoc[ -]*2(?:\s+type\s+(?:ii?|[12]))?\s+certif(?:ied|ication|ications)\b|\bcertif(?:ied|ication)\s+(?:for\s+|under\s+)?soc[ -]*2\b`)
var vendorNarrativeRating = regexp.MustCompile(`(?i)\b(?:low|moderate|high|unknown)[ -]risk\b|\brisk\s*(?:rating|assessment)?\s*(?:is|:|=)\s*(?:low|moderate|high|unknown)\b`)

// ValidateVendorNarrative catches the pilot's known terminology/rating failures.
// It is deliberately a bounded wording check, not a claim of semantic truth.
func ValidateVendorNarrative(report *WorkPackage, diagnosis string, assessment VendorAssessment) error {
	if report == nil {
		return errors.New("vendor report is required")
	}
	texts := []string{diagnosis, report.Summary, report.RequesterSummary, report.EscalationReason, report.ExpectedImpact}
	texts = append(texts, report.Unknowns...)
	for _, f := range report.Findings {
		texts = append(texts, f.Statement)
	}
	for _, c := range assessment.Criteria {
		texts = append(texts, c.Reason)
	}
	for _, text := range texts {
		if vendorCertificationClaim.MatchString(text) {
			return errors.New("vendor report must describe SOC 2 as an attestation or report")
		}
		if vendorNarrativeRating.MatchString(text) {
			return errors.New("vendor narrative must not assign an overall risk rating")
		}
	}
	return nil
}
