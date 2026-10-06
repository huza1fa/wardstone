package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func validWorkPackage() *WorkPackage {
	return &WorkPackage{Version: 1, Summary: "Account exists; entitlement needs review.", RequesterSummary: "Your access request is ready for review.",
		Findings: []Finding{{Kind: "verified", Statement: "The account exists.", EvidenceIDs: []EvidenceID{"ev_1"}}, {Kind: "assumption", Statement: "The requested group may be appropriate."}},
		Unknowns: []string{"Whether the manager approved access."}}
}

func TestWorkPackageValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*WorkPackage)
	}{
		{"version", func(w *WorkPackage) { w.Version = 2 }},
		{"empty summary", func(w *WorkPackage) { w.Summary = " " }},
		{"UTF8", func(w *WorkPackage) { w.RequesterSummary = "\xff" }},
		{"NUL", func(w *WorkPackage) { w.ExpectedImpact = "impact\x00" }},
		{"text limit", func(w *WorkPackage) { w.Summary = strings.Repeat("x", 12001) }},
		{"unlabeled assumption", func(w *WorkPackage) { w.Findings[1].Kind = "inferred" }},
		{"verified without citation", func(w *WorkPackage) { w.Findings[0].EvidenceIDs = nil }},
		{"unknown citation", func(w *WorkPackage) { w.Findings[0].EvidenceIDs = []EvidenceID{"hidden"} }},
		{"assumption unknown citation", func(w *WorkPackage) { w.Findings[1].EvidenceIDs = []EvidenceID{"hidden"} }},
		{"duplicate citation", func(w *WorkPackage) { w.Findings[0].EvidenceIDs = []EvidenceID{"ev_1", "ev_1"} }},
		{"duplicate finding", func(w *WorkPackage) { w.Findings = append(w.Findings, w.Findings[0]) }},
		{"duplicate unknown", func(w *WorkPackage) { w.Unknowns = append(w.Unknowns, " "+w.Unknowns[0]+" ") }},
		{"finding count", func(w *WorkPackage) { w.Findings = make([]Finding, MaxWorkPackageFindings+1) }},
		{"unknown count", func(w *WorkPackage) { w.Unknowns = make([]string, MaxWorkPackageUnknowns+1) }},
		{"citation count", func(w *WorkPackage) { w.Findings[0].EvidenceIDs = make([]EvidenceID, 33) }},
		{"total citation count", func(w *WorkPackage) {
			w.Findings = nil
			for i := 0; i < 5; i++ {
				finding := Finding{Kind: "verified", Statement: fmt.Sprintf("Finding %d", i)}
				for j := 0; j < 26; j++ {
					finding.EvidenceIDs = append(finding.EvidenceIDs, EvidenceID(fmt.Sprintf("ev_%d", j)))
				}
				w.Findings = append(w.Findings, finding)
			}
		}},
		{"total bytes", func(w *WorkPackage) {
			w.Findings = nil
			for i := 0; i < 20; i++ {
				w.Findings = append(w.Findings, Finding{Kind: "assumption", Statement: fmt.Sprintf("%d %s", i, strings.Repeat("x", 3900))})
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := validWorkPackage()
			test.mutate(w)
			if err := w.Validate([]Evidence{{ID: "ev_1"}}); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
	if err := validWorkPackage().Validate([]Evidence{{ID: "ev_1"}}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkPackageCloneAndCanonicalRoundTrip(t *testing.T) {
	t.Parallel()
	w := validWorkPackage()
	clone := w.Clone()
	clone.Findings[0].EvidenceIDs[0] = "changed"
	clone.Findings[0].Statement = "changed"
	clone.Unknowns[0] = "changed"
	if w.Findings[0].EvidenceIDs[0] != "ev_1" || w.Unknowns[0] == "changed" || w.Findings[0].Statement == "changed" {
		t.Fatal("clone aliases report")
	}
	for _, original := range []*WorkPackage{w, {Version: 1, Summary: "No observations; clarify request."}} {
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded WorkPackage
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("round trip %s: %v", data, err)
		}
	}
	var absent *WorkPackage
	if absent.Clone() != nil {
		t.Fatal("nil clone")
	}
}

func TestWorkPackageStrictJSON(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		`null`, `[]`, `{"version":1,"summary":"ok","summary":"overwritten"}`,
		`{"version":1,"summary":"ok","Summary":"alias"}`,
		`{"version":1,"summary":"ok","unknowns":null}`,
		`{"version":1,"summary":"ok","findings":[{"kind":"assumption","statement":"a","statement":"b"}]}`,
		`{"version":1,"summary":"ok","findings":[{"kind":"assumption","statement":"a","Kind":"verified"}]}`,
		`{"version":1,"summary":"ok","extra":true}`,
		`{"version":1,"summary":"ok"} {}`,
	} {
		var report WorkPackage
		if err := json.Unmarshal([]byte(content), &report); err == nil {
			t.Fatalf("accepted %s", content)
		}
	}
}
