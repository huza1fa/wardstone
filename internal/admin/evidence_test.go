package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestResearchEvidenceProjection(t *testing.T) {
	t.Parallel()
	item := domain.Evidence{ID: "evidence-1", InvestigationID: "inv-1", Source: "research", Capability: "research.vendor.collect",
		Kind: "research.vendor_document", ObservedAt: time.Now().UTC(), Data: json.RawMessage(`{"title":"Policy","content":"<script>untrusted</script>","url":"https://example.com/policy","content_sha256":"abc","claim_type":"vendor_claim","truncated":true,"format":"pdf","page_number":2,"page_count":3,"path":"/private/report.pdf","token":"secret","unknown":"do not expose"}`)}
	view, err := ProjectResearchEvidence(item)
	if err != nil || view.ID != item.ID || view.InvestigationID != item.InvestigationID || view.Document.Content != "<script>untrusted</script>" || view.Document.PageNumber != 2 || !view.Document.Truncated {
		t.Fatalf("projection = %+v, %v", view, err)
	}
	encoded, err := json.Marshal(view)
	if err != nil || bytes.Contains(encoded, []byte("secret")) || bytes.Contains(encoded, []byte("private")) || bytes.Contains(encoded, []byte("unknown")) || bytes.Contains(encoded, []byte("<script>")) {
		t.Fatalf("unsafe projection: %s, %v", encoded, err)
	}
	for _, rawURL := range []string{"file:///private/report.pdf", "https://user:secret@example.com", "https://example.com?token=secret", "https://example.com/#secret", "javascript:alert(1)", "%"} {
		t.Run(rawURL, func(t *testing.T) {
			copy := item
			copy.Data, _ = json.Marshal(ResearchDocument{Title: "Policy", URL: rawURL, Content: "source"})
			got, err := ProjectResearchEvidence(copy)
			if err != nil || got.Document.URL != "" || got.Document.Content != "source" {
				t.Fatalf("unsafe URL not withheld: %+v %v", got, err)
			}
		})
	}
	item.Kind = "research.document_unavailable"
	item.Data = json.RawMessage(`{"title":"Scanned report","status":"pdf_pages_without_text","content":"must not display","affected_pages":[1,3]}`)
	view, err = ProjectResearchEvidence(item)
	if err != nil || view.Document.Content != "" || view.Document.Status != "pdf_pages_without_text" || len(view.Document.AffectedPages) != 2 {
		t.Fatalf("gap projection = %+v %v", view, err)
	}
}

func TestResearchEvidenceProjectionRejectsUnsupportedAndOversizedData(t *testing.T) {
	t.Parallel()
	valid := domain.Evidence{ID: "evidence-1", InvestigationID: "inv-1", Source: "research", Capability: "research.knowledge.collect", Kind: "research.knowledge_document", Data: json.RawMessage(`{"title":"Runbook","content":"Steps"}`)}
	for _, test := range []struct {
		name string
		edit func(*domain.Evidence)
	}{
		{"missing evidence ID", func(e *domain.Evidence) { e.ID = "" }},
		{"missing attempt ID", func(e *domain.Evidence) { e.InvestigationID = "" }},
		{"other connector", func(e *domain.Evidence) { e.Source = "google" }},
		{"legacy provenance", func(e *domain.Evidence) { e.Capability = "" }},
		{"mismatched capability", func(e *domain.Evidence) { e.Capability = "research.vendor.collect" }},
		{"selector is not a document", func(e *domain.Evidence) { e.Kind = "research.request_context" }},
		{"unknown kind", func(e *domain.Evidence) { e.Kind = "research.new_document" }},
		{"null", func(e *domain.Evidence) { e.Data = json.RawMessage(" null \n") }},
		{"malformed", func(e *domain.Evidence) { e.Data = json.RawMessage("{") }},
		{"wrong content type", func(e *domain.Evidence) { e.Data = json.RawMessage(`{"content":42}`) }},
		{"oversized payload", func(e *domain.Evidence) { e.Data = json.RawMessage(strings.Repeat(" ", MaxResearchEvidenceData+1)) }},
		{"oversized excerpt", func(e *domain.Evidence) {
			e.Data, _ = json.Marshal(ResearchDocument{Content: strings.Repeat("x", (24<<10)+1)})
		}},
		{"oversized metadata", func(e *domain.Evidence) { e.Data, _ = json.Marshal(ResearchDocument{Title: strings.Repeat("x", 257)}) }},
		{"invalid page", func(e *domain.Evidence) { e.Data = json.RawMessage(`{"page_number":-1}`) }},
		{"excessive pages", func(e *domain.Evidence) { e.Data = json.RawMessage(`{"page_count":21}`) }},
		{"invalid affected page", func(e *domain.Evidence) { e.Data = json.RawMessage(`{"affected_pages":[0]}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := valid
			test.edit(&item)
			if _, err := ProjectResearchEvidence(item); !errors.Is(err, ErrEvidenceNotFound) {
				t.Fatalf("got %v, want unavailable", err)
			}
		})
	}
	// The largest supported, maximally HTML-escaped excerpt remains bounded.
	valid.Data, _ = json.Marshal(ResearchDocument{Content: strings.Repeat("<", 24<<10)})
	view, err := ProjectResearchEvidence(valid)
	encoded, _ := json.Marshal(view)
	if err != nil || len(encoded) >= 256<<10 {
		t.Fatalf("maximum excerpt: bytes=%d err=%v", len(encoded), err)
	}
}
