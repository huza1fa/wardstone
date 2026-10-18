package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wardstone-project/wardstone/internal/agent"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/connectors/jira"
	"github.com/wardstone-project/wardstone/internal/connectors/research"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

func TestVendorRiskReportEndToEnd(t *testing.T) {
	for _, scenario := range []string{"complete", "missing scope", "failed source", "truncated source"} {
		t.Run(scenario, func(t *testing.T) {
			pool := routingTestDatabase(t)
			remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "failed source" && r.URL.Path == "/audit" {
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				if scenario == "truncated source" && r.URL.Path == "/vendor" {
					w.Write([]byte(strings.Repeat("document ", 4000)))
					return
				}
				w.Write([]byte("Synthetic scoped source describing identity controls and assurance findings."))
			}))
			defer remote.Close()
			collector, err := research.NewCollector(research.VendorCollect, research.Catalog{"Acme": {
				{Title: "Vendor security claims", URL: remote.URL + "/vendor"},
				{Title: "Designated independent report", URL: remote.URL + "/audit", ClaimType: "independent_report"},
			}}, remote.Client())
			if err != nil {
				t.Fatal(err)
			}
			riskConfig := domain.VendorRiskConfig{Rubric: &domain.VendorRubric{Version: "pilot-v1", ModerateAt: 25, HighAt: 60, Criteria: []domain.VendorCriterion{
				{ID: "identity", Requirement: "Scoped identity controls", Weight: 40, Evidence: "vendor_claim"},
				{ID: "assurance", Requirement: "Scoped independent assurance", Weight: 60, Evidence: "independent_report"},
			}}, Deployments: map[string]domain.VendorDeployment{"acme": {UseCase: "Internal documentation", DataClassification: "Internal", AccessScope: "Read only"}}}
			if scenario == "missing scope" {
				riskConfig.Deployments = nil
			}
			model := &pilotModel{respond: func(r agent.Request) agent.Result {
				if r.VendorReview == nil || r.VendorReview.Rubric.Version != "pilot-v1" || r.VendorReview.Vendor != "acme" {
					t.Fatalf("missing scoring context: %+v", r.VendorReview)
				}
				criteria := []domain.VendorCriterionAssessment{}
				findings := []domain.Finding{}
				for _, criterion := range r.VendorReview.Rubric.Criteria {
					assessment := domain.VendorCriterionAssessment{ID: criterion.ID, Status: "unknown", Reason: "Required source not available", EvidenceIDs: []domain.EvidenceID{}}
					for _, e := range r.Evidence {
						if e.Kind != "research.vendor_document" {
							continue
						}
						var doc struct {
							ClaimType string `json:"claim_type"`
						}
						_ = json.Unmarshal(e.Data, &doc)
						if (criterion.ID == "identity" && doc.ClaimType == "vendor_claim") || (criterion.ID == "assurance" && doc.ClaimType == "independent_report") {
							assessment.Status = "met"
							if criterion.ID == "assurance" {
								assessment.Status = "partial"
							}
							assessment.Reason = "The cited source describes the scoped controls."
							assessment.EvidenceIDs = []domain.EvidenceID{e.ID}
							if criterion.ID == "identity" {
								findings = append(findings, domain.Finding{Kind: "verified", Statement: "The vendor describes scoped identity controls.", EvidenceIDs: []domain.EvidenceID{e.ID}})
							}
						}
					}
					criteria = append(criteria, assessment)
				}
				// A custom provider must not be able to rewrite the scoring authority.
				r.VendorReview.Rubric.Version = "provider-poison"
				r.VendorReview.Rubric.Criteria[0].Weight = 100
				r.VendorReview.Deployment.AccessScope = "Administrator"
				return agent.Result{WorkPackage: &domain.WorkPackage{Version: 1, Summary: "Acme source review", Findings: findings, RequesterSummary: "Untrusted model requester wording"}, VendorAssessment: &domain.VendorAssessment{Criteria: criteria}}
			}}
			profiles := []specialists.Profile{{Name: specialists.HelpDesk, Instructions: "Help"}, {Name: specialists.VendorReview, Instructions: "Review vendor evidence", AllowedCollectors: []domain.CapabilityName{research.VendorCollect}}}
			rules := []specialists.RouteRule{{Name: "vendor", Specialist: specialists.VendorReview, Classification: "vendor_review", Match: specialists.RouteMatch{RequestTypes: []string{"Vendor review"}}}}
			store, service, server := pilotService(t, pool, model, []connectors.EvidenceCollector{collector}, profiles, rules, riskConfig)
			riskConfig.Rubric.Version = "caller-poison"
			riskConfig.Rubric.Criteria[0].Weight = 1
			var intake struct {
				ID domain.InvestigationID `json:"investigation_id"`
			}
			pilotHTTP(t, server, "POST", "/v1/tickets/jira", "pilot-webhook", jira.TicketPayload{ExternalID: "VENDOR-" + scenario, Summary: "Review Acme", RequestType: "Vendor review", Fields: map[string][]string{"Vendor": {"Acme"}, "Access scope": {"Administrator"}}}, 202, &intake)
			pilotRun(t, store, service, intake.ID)
			stored := database.NewStore(pool)
			item, err := stored.GetInvestigation(context.Background(), intake.ID)
			if err != nil || item.WorkPackage == nil || item.WorkPackage.RiskAssessment == nil {
				t.Fatalf("report=%+v err=%v", item, err)
			}
			risk := item.WorkPackage.RiskAssessment
			expected := "UNKNOWN"
			if scenario == "complete" {
				expected = "MODERATE"
			}
			if risk.Rating != expected || risk.Scoring.Context.Rubric.Version != "pilot-v1" || risk.Scoring.Context.Rubric.Criteria[0].Weight != 40 {
				t.Fatalf("unexpected scoring authority: %+v", risk)
			}
			if scenario == "complete" && (risk.Scoring.Score == nil || *risk.Scoring.Score != 30 || risk.Scoring.Context.Deployment.AccessScope != "Read only") {
				t.Fatal("deployment scope or score changed")
			}
			if item.WorkPackage.Findings[0].Kind != "vendor_claim" {
				t.Fatal("vendor claims labelled independently verified")
			}
			messages, _ := stored.ListMessages(context.Background(), intake.ID)
			if len(messages) != 1 || !strings.Contains(messages[0].Body, "Provisional vendor risk: "+expected) || strings.Contains(messages[0].Body, "Untrusted model requester wording") {
				t.Fatalf("unbound requester rating: %+v", messages)
			}
			events, _ := stored.Timeline(context.Background(), intake.ID)
			count := 0
			for _, event := range events {
				if event.Type == audit.VendorRiskScored {
					count++
					if event.ActorType != audit.ActorSystem || !strings.Contains(string(event.Data), risk.Scoring.RubricDigest) {
						t.Fatal("scoring attributed to model or missing rubric digest")
					}
				}
			}
			if count != 1 {
				t.Fatal("deterministic scoring was not audited once")
			}
			if deliveries, _ := stored.ListDeliveries(context.Background(), intake.ID); len(deliveries) != 0 {
				t.Fatal("vendor review bypassed requester-message approval")
			}
		})
	}
}
