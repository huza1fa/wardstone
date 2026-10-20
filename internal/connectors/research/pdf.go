package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const (
	MaxPDFDownload = 8 << 20
	MaxPDFPages    = 20
	maxPDFOutput   = 256 << 10
	pdfTimeout     = 5 * time.Second
)

type pdfResult struct {
	pages  []string
	status string
}
type pdfCommand func(context.Context, []byte, string, ...string) ([]byte, string)

// Both commands consume the same immutable byte snapshot. No source filename,
// caller credentials, or model-chosen arguments enter the parser process.
func extractPDF(ctx context.Context, data []byte, run pdfCommand) pdfResult {
	if !strings.HasPrefix(string(data[:min(len(data), 8)]), "%PDF-") {
		return pdfResult{status: "pdf_malformed"}
	}
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	info, status := run(ctx, data, "pdfinfo", "-enc", "UTF-8", "-")
	if status != "" {
		return pdfResult{status: status}
	}
	pages := 0
	for _, line := range strings.Split(string(info), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages":
			pages, _ = strconv.Atoi(strings.TrimSpace(value))
		case "Encrypted":
			if strings.HasPrefix(strings.TrimSpace(value), "yes") {
				return pdfResult{status: "pdf_encrypted"}
			}
		}
	}
	if pages < 1 {
		return pdfResult{status: "pdf_malformed"}
	}
	if pages > MaxPDFPages {
		return pdfResult{status: "pdf_page_limit"}
	}
	text, status := run(ctx, data, "pdftotext", "-f", "1", "-l", strconv.Itoa(pages), "-enc", "UTF-8", "-eol", "unix", "-", "-")
	if status != "" {
		return pdfResult{status: status}
	}
	if !utf8.Valid(text) || strings.ContainsRune(string(text), 0) {
		return pdfResult{status: "pdf_unreadable"}
	}
	parts := strings.Split(string(text), "\f")
	// Poppler emits one form feed per page, including empty pages.
	if len(parts) != pages+1 || strings.TrimSpace(parts[len(parts)-1]) != "" {
		return pdfResult{status: "pdf_malformed"}
	}
	return pdfResult{pages: parts[:pages]}
}

func (c *Collector) pdfEvidence(ctx context.Context, source Source, resource string, data []byte, modified string) []domain.Evidence {
	result := extractPDF(ctx, data, runPDFCommand)
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	metadata := func() map[string]any {
		d := map[string]any{"title": source.Title, "format": "pdf", "content_sha256": hash, "resource": resource, "extraction_method": "poppler-text-v1"}
		if len(result.pages) > 0 {
			d["page_count"] = len(result.pages)
		}
		return d
	}
	gap := func(status string, pages ...int) domain.Evidence {
		d := metadata()
		d["status"] = status
		d["unknown"] = "PDF evidence is incomplete; an operator must inspect or replace this document."
		if len(pages) > 0 {
			d["affected_pages"] = pages
		}
		return c.evidence("research.document_unavailable", source.Title+" has incomplete PDF evidence", d)
	}
	if result.status != "" {
		return []domain.Evidence{gap(result.status)}
	}
	kind, claim := "research.vendor_document", source.ClaimType
	if claim == "" {
		claim = "vendor_claim"
	}
	if c.capability == KnowledgeCollect {
		kind, claim = "research.knowledge_document", "administrator_document"
	}
	evidence := []domain.Evidence{}
	remaining := MaxContent
	var missingText, omitted []int
	for i, page := range result.pages {
		content := strings.TrimSpace(page)
		if content == "" {
			missingText = append(missingText, i+1)
			continue
		}
		if remaining == 0 {
			omitted = append(omitted, i+1)
			continue
		}
		truncated := len(content) > remaining
		if truncated {
			content = content[:remaining]
			for !utf8.ValidString(content) {
				content = content[:len(content)-1]
			}
		}
		remaining -= len(content)
		if content == "" {
			omitted = append(omitted, i+1)
			continue
		}
		d := metadata()
		d["page_number"], d["content"], d["last_modified"], d["truncated"], d["claim_type"] = i+1, content, modified, truncated, claim
		evidence = append(evidence, c.evidence(kind, fmt.Sprintf("%s — page %d", source.Title, i+1), d))
	}
	// One gap per failure category keeps the failure record bounded even when
	// every page has no text. No OCR or completeness claims are inferred.
	if len(missingText) > 0 {
		evidence = append(evidence, gap("pdf_pages_without_text", missingText...))
	}
	if len(omitted) > 0 {
		evidence = append(evidence, gap("pdf_excerpt_limit", omitted...))
	}
	return evidence
}
