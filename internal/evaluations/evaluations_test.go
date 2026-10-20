package evaluations

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func dataset(t *testing.T) Dataset {
	t.Helper()
	f, err := os.Open("../../evaluations/pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type provider struct {
	run func(context.Context, agent.Request) (agent.Result, error)
}

func (provider) Name() domain.ModelProviderName { return "evaluation-test" }
func (provider) Model() string                  { return "test-model" }
func (p provider) Diagnose(ctx context.Context, r agent.Request) (agent.Result, error) {
	return p.run(ctx, r)
}
func TestRecordedEvaluationBaseline(t *testing.T) {
	d := dataset(t)
	if len(d.Cases) != 7 {
		t.Fatal("seed case set changed; review evaluation coverage")
	}
	for _, c := range d.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p := RecordedProvider{Results: map[domain.InvestigationID]agent.Result{c.Request.InvestigationID: *c.RecordedResult}}
			r := Run(context.Background(), c, p, time.Second, false)
			if !r.Passed || !r.CitationValid || !r.ReportValid || r.Response != nil {
				t.Fatalf("baseline=%+v", r)
			}
			if c.Expect.Rating == "UNKNOWN" && r.Score != nil {
				t.Fatal("incomplete evaluation got score")
			}
		})
	}
}
func TestEvaluationRejectsBadCitationsWordingAndFalseCompleteness(t *testing.T) {
	for _, scenario := range []string{"unknown citation", "wrong vendor provenance", "forbidden wording", "missing disclosure", "model overall rating", "unexpected action"} {
		t.Run(scenario, func(t *testing.T) {
			c := dataset(t).Cases[0]
			result := *c.RecordedResult
			switch scenario {
			case "unknown citation":
				result.VendorAssessment.Criteria[0].EvidenceIDs = []domain.EvidenceID{"invented"}
			case "wrong vendor provenance":
				c.Request.Evidence[1].Source = "google"
			case "forbidden wording":
				result.WorkPackage.Summary = "The vendor is SOC 2 certified"
			case "missing disclosure":
				c.Expect.RequireUnknowns = true
			case "model overall rating":
				result.WorkPackage.RiskAssessment = &domain.RiskAssessment{Rating: "LOW", Rationale: "Provider supplied rating"}
			case "unexpected action":
				result.Actions = []agent.ProposedAction{{Capability: "google.groups.add_member"}}
			}
			p := provider{run: func(context.Context, agent.Request) (agent.Result, error) { return result, nil }}
			out := Run(context.Background(), c, p, time.Second, false)
			if out.Passed || len(out.Failures) == 0 {
				t.Fatalf("accepted %s: %+v", scenario, out)
			}
			if (scenario == "unknown citation" || scenario == "wrong vendor provenance") && out.CitationValid {
				t.Fatal("bad citation metric")
			}
		})
	}
}
func TestEvaluationFreezesProviderAuthorityAndBoundsCalls(t *testing.T) {
	c := dataset(t).Cases[0]
	p := provider{run: func(_ context.Context, r agent.Request) (agent.Result, error) {
		r.VendorReview.Rubric.Criteria[1].Weight = 100
		r.VendorReview.Deployment.AccessScope = "provider mutation"
		r.Evidence[0].Data = json.RawMessage(`{}`)
		return *c.RecordedResult, nil
	}}
	out := Run(context.Background(), c, p, time.Second, true)
	if !out.Passed || out.Score == nil || *out.Score != 30 || out.Response == nil || c.Request.VendorReview.Deployment.AccessScope != "Read only" {
		t.Fatalf("authority changed: %+v", out)
	}
	slow := provider{run: func(ctx context.Context, _ agent.Request) (agent.Result, error) {
		<-ctx.Done()
		return agent.Result{}, errors.New("synthetic-secret-must-not-be-printed")
	}}
	out = Run(context.Background(), c, slow, time.Millisecond, false)
	raw, _ := json.Marshal(out)
	if out.Passed || strings.Contains(string(raw), "synthetic-secret") || len(out.Failures) != 1 || out.Failures[0] != "provider_call_failed" {
		t.Fatalf("timeout output=%s", raw)
	}
}
func TestSavedDatasetValidation(t *testing.T) {
	d := dataset(t)
	for _, scenario := range []string{"duplicate case", "unknown field", "oversize", "invalid rubric", "missing citation"} {
		t.Run(scenario, func(t *testing.T) {
			raw, _ := json.Marshal(d)
			var copy Dataset
			_ = json.Unmarshal(raw, &copy)
			switch scenario {
			case "duplicate case":
				copy.Cases = append(copy.Cases, copy.Cases[0])
			case "invalid rubric":
				copy.Cases[0].Request.VendorReview.Rubric.Criteria[0].Weight = 1
			case "missing citation":
				copy.Cases[0].Expect.RequiredEvidenceIDs = []domain.EvidenceID{"missing"}
			}
			raw, _ = json.Marshal(copy)
			if scenario == "unknown field" {
				raw = append([]byte(`{"unknown":true,`), raw[1:]...)
			}
			if scenario == "oversize" {
				raw = []byte(strings.Repeat("x", MaxDatasetBytes+1))
			}
			if _, err := Read(strings.NewReader(string(raw))); err == nil {
				t.Fatal("invalid dataset accepted")
			}
		})
	}
}

func TestOversizedProviderDiagnosisFailsSharedBoundaryAndCaptureBudget(t *testing.T) {
	c := dataset(t).Cases[0]
	result := *c.RecordedResult
	result.Diagnosis = strings.Repeat("x", 300<<10)
	p := provider{run: func(context.Context, agent.Request) (agent.Result, error) { return result, nil }}
	out := Run(context.Background(), c, p, time.Second, true)
	if out.Passed || out.ReportValid || out.Response != nil || !strings.Contains(strings.Join(out.Failures, ","), "response_capture_limit") {
		t.Fatalf("unbounded evaluation response: %+v", out)
	}
}
