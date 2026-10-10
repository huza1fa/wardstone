package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	WorkPackageVersion      = 1
	MaxWorkPackageBytes     = 64 << 10
	MaxWorkPackageFindings  = 32
	MaxWorkPackageUnknowns  = 32
	MaxWorkPackageCitations = 128
)

type WorkPackage struct {
	Version          int             `json:"version"`
	Summary          string          `json:"summary"`
	Findings         []Finding       `json:"findings"`
	Unknowns         []string        `json:"unknowns"`
	EscalationReason string          `json:"escalation_reason"`
	RequesterSummary string          `json:"requester_summary"`
	ExpectedImpact   string          `json:"expected_impact"`
	RiskAssessment   *RiskAssessment `json:"risk_assessment,omitempty"`
}

// A model-generated risk rating is a provisional assessment of cited sources,
// never business authorization or an independent certification.
type RiskAssessment struct {
	Rating      string       `json:"rating"`
	Rationale   string       `json:"rationale"`
	EvidenceIDs []EvidenceID `json:"evidence_ids"`
}

type Finding struct {
	Kind        string       `json:"kind"` // verified or assumption
	Statement   string       `json:"statement"`
	EvidenceIDs []EvidenceID `json:"evidence_ids"`
}

func (w *WorkPackage) Clone() *WorkPackage {
	if w == nil {
		return nil
	}
	copy := *w
	copy.Unknowns = append([]string(nil), w.Unknowns...)
	if w.RiskAssessment != nil {
		risk := *w.RiskAssessment
		risk.EvidenceIDs = append([]EvidenceID(nil), risk.EvidenceIDs...)
		copy.RiskAssessment = &risk
	}
	if w.Findings != nil {
		copy.Findings = make([]Finding, len(w.Findings))
		for i, finding := range w.Findings {
			copy.Findings[i] = finding
			copy.Findings[i].EvidenceIDs = append([]EvidenceID(nil), finding.EvidenceIDs...)
		}
	}
	return &copy
}

// MarshalJSON keeps empty collections canonical and round-trippable through
// strict decoding, without altering the caller's slices.
func (w WorkPackage) MarshalJSON() ([]byte, error) {
	copy := (&w).Clone()
	if copy.Findings == nil {
		copy.Findings = []Finding{}
	}
	if copy.Unknowns == nil {
		copy.Unknowns = []string{}
	}
	for i := range copy.Findings {
		if copy.Findings[i].EvidenceIDs == nil {
			copy.Findings[i].EvidenceIDs = []EvidenceID{}
		}
	}
	if copy.RiskAssessment != nil && copy.RiskAssessment.EvidenceIDs == nil {
		copy.RiskAssessment.EvidenceIDs = []EvidenceID{}
	}
	type plain WorkPackage
	return json.Marshal((*plain)(copy))
}

// Validate checks both structure and citations against exactly the observations
// supplied to the model. A stored but undisclosed observation is not a citation.
func (w *WorkPackage) Validate(supplied []Evidence) error {
	if err := w.ValidateStructure(); err != nil {
		return err
	}
	known := make(map[EvidenceID]bool, len(supplied))
	for _, item := range supplied {
		if item.ID != "" {
			known[item.ID] = true
		}
	}
	for _, finding := range w.Findings {
		for _, id := range finding.EvidenceIDs {
			if !known[id] {
				return fmt.Errorf("finding references unknown evidence %q", id)
			}
		}
	}
	if w.RiskAssessment != nil {
		for _, id := range w.RiskAssessment.EvidenceIDs {
			if !known[id] {
				return fmt.Errorf("risk assessment references unknown evidence %q", id)
			}
		}
		if w.RiskAssessment.Rating != "UNKNOWN" {
			hasDocument := false
			for _, item := range supplied {
				for _, id := range w.RiskAssessment.EvidenceIDs {
					if item.ID == id && item.Kind == "research.vendor_document" {
						hasDocument = true
					}
				}
			}
			if !hasDocument {
				return errors.New("known vendor risk rating requires collected vendor document evidence")
			}
		}
	}
	return nil
}

// ValidateStructure is also useful at persistence boundaries without prompt context.
func (w *WorkPackage) ValidateStructure() error {
	if w == nil {
		return errors.New("work package is required")
	}
	if w.Version != WorkPackageVersion {
		return errors.New("unsupported work package version")
	}
	if len(w.Findings) > MaxWorkPackageFindings || len(w.Unknowns) > MaxWorkPackageUnknowns {
		return errors.New("work package exceeds finding or unknown count limit")
	}
	for _, field := range []struct {
		name, value string
		limit       int
		required    bool
	}{
		{"work package summary", w.Summary, 12000, true},
		{"escalation reason", w.EscalationReason, 2000, false},
		{"requester summary", w.RequesterSummary, 12000, false},
		{"expected impact", w.ExpectedImpact, 4000, false},
	} {
		if err := validateText(field.name, field.value, field.limit, field.required); err != nil {
			return err
		}
	}
	statements := make(map[string]bool)
	citations := 0
	for _, finding := range w.Findings {
		if finding.Kind != "verified" && finding.Kind != "assumption" {
			return errors.New("finding kind must be verified or assumption")
		}
		if err := validateText("finding statement", finding.Statement, 4000, true); err != nil {
			return err
		}
		key := strings.TrimSpace(finding.Statement)
		if statements[key] {
			return errors.New("duplicate finding statement")
		}
		statements[key] = true
		if finding.Kind == "verified" && len(finding.EvidenceIDs) == 0 {
			return errors.New("verified finding requires evidence citations")
		}
		if len(finding.EvidenceIDs) > 32 {
			return errors.New("finding exceeds citation count limit")
		}
		seen := make(map[EvidenceID]bool)
		for _, id := range finding.EvidenceIDs {
			if err := validateText("evidence ID", string(id), 128, true); err != nil {
				return err
			}
			if strings.TrimSpace(string(id)) != string(id) || seen[id] {
				return errors.New("duplicate or noncanonical evidence ID")
			}
			seen[id] = true
			citations++
		}
	}
	if citations > MaxWorkPackageCitations {
		return errors.New("work package exceeds total citation count limit")
	}
	unknowns := make(map[string]bool)
	if risk := w.RiskAssessment; risk != nil {
		if risk.Rating != "LOW" && risk.Rating != "MODERATE" && risk.Rating != "HIGH" && risk.Rating != "UNKNOWN" {
			return errors.New("risk rating must be LOW, MODERATE, HIGH, or UNKNOWN")
		}
		if err := validateText("risk rationale", risk.Rationale, 4000, true); err != nil {
			return err
		}
		if len(risk.EvidenceIDs) > 32 || (risk.Rating != "UNKNOWN" && len(risk.EvidenceIDs) == 0) {
			return errors.New("known risk rating requires bounded citations")
		}
		seen := make(map[EvidenceID]bool)
		for _, id := range risk.EvidenceIDs {
			if err := validateText("risk evidence ID", string(id), 128, true); err != nil {
				return err
			}
			if strings.TrimSpace(string(id)) != string(id) || seen[id] {
				return errors.New("duplicate or noncanonical risk evidence ID")
			}
			seen[id] = true
			citations++
		}
		if citations > MaxWorkPackageCitations {
			return errors.New("work package exceeds total citation count limit")
		}
	}
	for _, unknown := range w.Unknowns {
		if err := validateText("unknown", unknown, 2000, true); err != nil {
			return err
		}
		key := strings.TrimSpace(unknown)
		if unknowns[key] {
			return errors.New("duplicate unknown")
		}
		unknowns[key] = true
	}
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	if len(data) > MaxWorkPackageBytes {
		return errors.New("work package exceeds total byte limit")
	}
	return nil
}

// UnmarshalJSON preserves strict report contracts even when embedded in a
// provider response: no duplicate, unknown, case-variant, or null fields.
func (w *WorkPackage) UnmarshalJSON(data []byte) error {
	if len(data) > MaxWorkPackageBytes || !utf8.Valid(data) {
		return errors.New("invalid work package encoding or size")
	}
	fields, err := strictFields(data, "version", "summary", "findings", "unknowns", "escalation_reason", "requester_summary", "expected_impact", "risk_assessment")
	if err != nil {
		return err
	}
	if raw, ok := fields["risk_assessment"]; ok {
		if _, err := strictFields(raw, "rating", "rationale", "evidence_ids"); err != nil {
			return err
		}
	}
	if raw, ok := fields["findings"]; ok {
		var findings []json.RawMessage
		if err := json.Unmarshal(raw, &findings); err != nil {
			return err
		}
		for _, finding := range findings {
			if _, err := strictFields(finding, "kind", "statement", "evidence_ids"); err != nil {
				return err
			}
		}
	}
	type plain WorkPackage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if err := (*WorkPackage)(&decoded).ValidateStructure(); err != nil {
		return err
	}
	*w = WorkPackage(decoded)
	return nil
}

func strictFields(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("report value must be an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		known := false
		for _, key := range allowed {
			known = known || name == key
		}
		if !ok || !known {
			return nil, errors.New("unknown or noncanonical report field")
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, errors.New("duplicate report field")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, errors.New("report fields must not be null")
		}
		fields[name] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing report data")
	}
	return fields, nil
}
