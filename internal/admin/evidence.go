package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

// MaxResearchEvidenceData bounds a single stored observation before decoding.
const MaxResearchEvidenceData = 256 << 10

var ErrEvidenceNotFound = errors.New("research evidence not found")

// ResearchEvidence is an operator-only projection, never a raw connector payload.
// Local paths, arbitrary connector fields, and original files are not exposed.
type ResearchEvidence struct {
	ID              domain.EvidenceID      `json:"id"`
	InvestigationID domain.InvestigationID `json:"investigation_id"`
	Capability      domain.CapabilityName  `json:"capability"`
	Kind            string                 `json:"kind"`
	ObservedAt      time.Time              `json:"observed_at"`
	Document        ResearchDocument       `json:"document"`
}

type ResearchDocument struct {
	Title            string `json:"title"`
	URL              string `json:"url,omitempty"`
	Content          string `json:"content"`
	ContentSHA256    string `json:"content_sha256,omitempty"`
	ClaimType        string `json:"claim_type,omitempty"`
	Truncated        bool   `json:"truncated"`
	Format           string `json:"format,omitempty"`
	PageNumber       int    `json:"page_number,omitempty"`
	PageCount        int    `json:"page_count,omitempty"`
	Status           string `json:"status,omitempty"`
	ExtractionMethod string `json:"extraction_method,omitempty"`
	AffectedPages    []int  `json:"affected_pages,omitempty"`
}

// ProjectResearchEvidence fails closed for unknown/legacy provenance. Bounds
// match the collectors and also cap the escaped JSON response well below 256 KiB.
func ProjectResearchEvidence(item domain.Evidence) (ResearchEvidence, error) {
	validKind := item.Kind == "research.document_unavailable" &&
		(item.Capability == "research.vendor.collect" || item.Capability == "research.knowledge.collect") ||
		item.Kind == "research.vendor_document" && item.Capability == "research.vendor.collect" ||
		item.Kind == "research.knowledge_document" && item.Capability == "research.knowledge.collect"
	if item.ID == "" || item.InvestigationID == "" || item.Source != "research" || !validKind || len(item.Data) > MaxResearchEvidenceData {
		return ResearchEvidence{}, ErrEvidenceNotFound
	}
	var document ResearchDocument
	if err := json.Unmarshal(item.Data, &document); err != nil || bytes.Equal(bytes.TrimSpace(item.Data), []byte("null")) {
		return ResearchEvidence{}, ErrEvidenceNotFound
	}
	if len(document.Title) > 256 || len(document.URL) > 2048 || len(document.Content) > 24<<10 ||
		len(document.ContentSHA256) > 64 || len(document.ClaimType) > 64 || len(document.Format) > 32 ||
		len(document.Status) > 128 || len(document.ExtractionMethod) > 64 ||
		document.PageNumber < 0 || document.PageNumber > 20 || document.PageCount < 0 || document.PageCount > 20 || len(document.AffectedPages) > 20 {
		return ResearchEvidence{}, ErrEvidenceNotFound
	}
	for _, page := range document.AffectedPages {
		if page < 1 || page > 20 {
			return ResearchEvidence{}, ErrEvidenceNotFound
		}
	}
	// Never turn malformed historical metadata into a credential-bearing URL or
	// filesystem location. URLs are displayed as text, not fetched or linked.
	if document.URL != "" {
		u, err := url.Parse(document.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			document.URL = ""
		}
	}
	if item.Kind == "research.document_unavailable" {
		document.Content = ""
	}
	return ResearchEvidence{ID: item.ID, InvestigationID: item.InvestigationID, Capability: item.Capability,
		Kind: item.Kind, ObservedAt: item.ObservedAt, Document: document}, nil
}
