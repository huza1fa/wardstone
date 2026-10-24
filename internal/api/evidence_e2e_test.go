package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/connectors/research"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestResearchEvidenceStoredSnapshotAndAttemptIsolation(t *testing.T) {
	pool := routingTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := database.NewStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	command := func(key string) cases.Command {
		return cases.Command{RequestID: key, Actor: "Evidence reviewer", Reason: "Verify stored sources"}
	}
	id, _, err := store.CreateCase(ctx, cases.Create{Command: command("create-evidence"), Summary: "Read VPN runbook", Metadata: domain.TicketMetadata{Fields: map[string][]string{"Knowledge topic": {"vpn"}}}}, "test", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartInvestigation(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runbook.md")
	const excerpt = "# VPN\nInspect the client logs first.\n<script>untrusted source text</script>"
	if err := os.WriteFile(path, []byte(excerpt), 0600); err != nil {
		t.Fatal(err)
	}
	collector, err := research.NewCollector(research.KnowledgeCollect, research.Catalog{"vpn": {{Title: "VPN runbook", Path: path}}}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.GetTicket(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	observations, err := collector.Collect(ctx, ticket)
	if err != nil || len(observations) != 2 {
		t.Fatalf("collection = %+v %v", observations, err)
	}
	textID := observations[1].ID
	pageID, gapID := domain.NewEvidenceID(), domain.NewEvidenceID()
	observations = append(observations,
		domain.Evidence{ID: pageID, Source: "research", Capability: research.VendorCollect, Kind: "research.vendor_document", ObservedAt: now, Data: json.RawMessage(`{"title":"Assurance report","format":"pdf","page_number":2,"page_count":3,"content":"Control scope on page two","claim_type":"independent_report","content_sha256":"file-hash","truncated":true,"path":"/private/report.pdf","api_key":"do-not-expose"}`)},
		domain.Evidence{ID: gapID, Source: "research", Capability: research.VendorCollect, Kind: "research.document_unavailable", ObservedAt: now, Data: json.RawMessage(`{"title":"Assurance report","format":"pdf","status":"pdf_pages_without_text","affected_pages":[3]}`)})
	if err := store.RecordEvidence(ctx, id, observations, nil, now); err != nil {
		t.Fatal(err)
	}
	// Inspection must not reread the file, depend on current source configuration,
	// or lose an older observation beyond the model-memory/list bound.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	newer := make([]domain.Evidence, 130)
	for i := range newer {
		newer[i] = domain.Evidence{ID: domain.NewEvidenceID(), Source: "research", Capability: research.KnowledgeCollect, Kind: "research.knowledge_document", ObservedAt: now.Add(time.Hour), Data: json.RawMessage(`{"content":"Newer observation"}`)}
	}
	if err := store.RecordEvidence(ctx, id, newer, nil, now); err != nil {
		t.Fatal(err)
	}
	server, err := api.NewServer(nil, store, "webhook", "operator", api.WithResearchEvidence(database.NewStore(pool)))
	if err != nil {
		t.Fatal(err)
	}
	read := func(attempt domain.InvestigationID, evidenceID domain.EvidenceID, status int) admin.ResearchEvidence {
		t.Helper()
		var result struct {
			Evidence admin.ResearchEvidence `json:"evidence"`
		}
		pilotHTTP(t, server, http.MethodGet, "/v1/investigations/"+string(attempt)+"/evidence/"+string(evidenceID), "operator", nil, status, &result)
		return result.Evidence
	}
	view := read(id, textID, http.StatusOK)
	if view.Document.Content != excerpt || view.Document.Title != "VPN runbook" || view.Document.ClaimType != "administrator_document" || view.Document.ContentSHA256 == "" {
		t.Fatalf("stored text snapshot = %+v", view)
	}
	view = read(id, pageID, http.StatusOK)
	if view.Document.PageNumber != 2 || view.Document.PageCount != 3 || !view.Document.Truncated || view.Document.Content != "Control scope on page two" || view.Document.ClaimType != "independent_report" {
		t.Fatalf("stored PDF page = %+v", view)
	}
	view = read(id, gapID, http.StatusOK)
	if view.Document.Content != "" || view.Document.Status != "pdf_pages_without_text" || len(view.Document.AffectedPages) != 1 {
		t.Fatalf("stored PDF gap = %+v", view)
	}
	if err := store.CancelCase(ctx, id, command("cancel-evidence"), now); err != nil {
		t.Fatal(err)
	}
	childID, _, err := store.RetryCase(ctx, id, command("retry-evidence"), "test", now)
	if err != nil {
		t.Fatal(err)
	}
	read(childID, textID, http.StatusNotFound)
	read(domain.NewInvestigationID(), textID, http.StatusNotFound)
	read(id, domain.NewEvidenceID(), http.StatusNotFound)
	read(id, textID, http.StatusOK)                   // Historical attempt links still work.
	read(id, observations[0].ID, http.StatusNotFound) // Selector is not a source.
	for _, test := range []struct{ source, capability, kind, data string }{
		{"google", "google.groups.read", "research.vendor_document", `{"content":"private directory payload"}`},
		{"research", "", "research.vendor_document", `{"content":"legacy provenance"}`},
		{"research", "research.knowledge.collect", "research.vendor_document", `{"content":"mismatched provenance"}`},
		{"research", "research.vendor.collect", "research.vendor_document", `{"content":42}`},
		{"research", "research.vendor.collect", "research.vendor_document", `{"content":"` + strings.Repeat("x", admin.MaxResearchEvidenceData) + `"}`},
	} {
		evidenceID := domain.NewEvidenceID()
		if _, err := pool.Exec(ctx, `INSERT INTO evidence(id, investigation_id, source, capability, kind, summary, data, observed_at) VALUES ($1,$2,$3,$4,$5,'',$6,$7)`, evidenceID, id, test.source, test.capability, test.kind, test.data, now); err != nil {
			t.Fatal(err)
		}
		read(id, evidenceID, http.StatusNotFound)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/investigations/"+string(id)+"/evidence/"+string(pageID), nil)
	request.Header.Set("Authorization", "Bearer operator")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	for _, forbidden := range []string{"/private/", "api_key", "do-not-expose"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("raw connector field leaked: %s", response.Body.String())
		}
	}
	// Source contents stay off the audit timeline, including after inspection.
	events, err := store.Timeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "Control scope on page two") || strings.Contains(string(encoded), "Inspect the client logs first") || strings.Contains(string(encoded), path) {
		t.Fatal("document body or local path leaked into timeline")
	}
}
