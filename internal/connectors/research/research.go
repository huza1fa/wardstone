// Package research collects bounded documents from administrator-owned source
// catalogs. Models and ticket text cannot choose URLs or filesystem paths.
package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
	"golang.org/x/net/html"
)

const (
	Name             domain.ConnectorName  = "research"
	VendorCollect    domain.CapabilityName = "research.vendor.collect"
	KnowledgeCollect domain.CapabilityName = "research.knowledge.collect"
	MaxDownload                            = 512 << 10
	MaxContent                             = 24 << 10
)

type Source struct {
	Title string `yaml:"title" json:"title"`
	URL   string `yaml:"url,omitempty" json:"url,omitempty"`
	Path  string `yaml:"path,omitempty" json:"-"`
}

type Catalog map[string][]Source

// ValidateCatalog copies and normalizes administrator configuration. Local
// paths are resolved relative to the policy file, never relative to a ticket.
func ValidateCatalog(input Catalog, base string, local bool) (Catalog, error) {
	if len(input) > 128 {
		return nil, errors.New("research catalog exceeds 128 resources")
	}
	result := make(Catalog, len(input))
	for key, sources := range input {
		name := strings.ToLower(strings.TrimSpace(key))
		if !validText(name, 128) || len(sources) < 1 || len(sources) > 4 {
			return nil, errors.New("research resources require a name and 1 through 4 sources")
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("duplicate normalized research resource")
		}
		for _, source := range sources {
			if !validText(source.Title, 256) || (source.URL == "") == (source.Path == "") {
				return nil, errors.New("research sources require a title and exactly one URL or path")
			}
			if source.URL != "" {
				u, err := url.Parse(source.URL)
				if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(source.URL) > 2048 {
					return nil, errors.New("research URLs must use HTTPS without credentials, query, or fragment")
				}
			} else {
				if !local || !validText(source.Path, 4096) {
					return nil, errors.New("local documents are permitted only in the knowledge catalog")
				}
				if ext := strings.ToLower(filepath.Ext(source.Path)); ext != ".md" && ext != ".txt" {
					return nil, errors.New("knowledge files must be Markdown or plain text")
				}
				if !filepath.IsAbs(source.Path) {
					source.Path = filepath.Join(base, source.Path)
				}
			}
			result[name] = append(result[name], source)
		}
	}
	return result, nil
}

func validText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func Capabilities() []capabilities.Definition {
	validate := func(data json.RawMessage) error {
		if strings.TrimSpace(string(data)) != "{}" {
			return errors.New("research collection accepts only an empty argument object")
		}
		return nil
	}
	return []capabilities.Definition{
		{Name: VendorCollect, Connector: Name, Description: "Read configured public vendor documents", Effect: domain.EffectRead, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validate},
		{Name: KnowledgeCollect, Connector: Name, Description: "Read configured systems knowledge documents", Effect: domain.EffectRead, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validate},
	}
}

type Collector struct {
	capability domain.CapabilityName
	field      string
	catalog    Catalog
	client     *http.Client
}

func NewCollector(capability domain.CapabilityName, catalog Catalog, client *http.Client) (*Collector, error) {
	if capability != VendorCollect && capability != KnowledgeCollect || client == nil {
		return nil, errors.New("research capability and HTTP client are required")
	}
	copy, err := ValidateCatalog(catalog, "", capability == KnowledgeCollect)
	if err != nil {
		return nil, err
	}
	field := "Vendor"
	if capability == KnowledgeCollect {
		field = "Knowledge topic"
	}
	return &Collector{capability: capability, field: field, catalog: copy, client: connectors.NoRedirectClient(client)}, nil
}

func (*Collector) Name() domain.ConnectorName          { return Name }
func (c *Collector) Capability() domain.CapabilityName { return c.capability }
func (c *Collector) Collect(ctx context.Context, ticket domain.Ticket) ([]domain.Evidence, error) {
	return c.CollectCase(ctx, ticket, nil)
}

func (c *Collector) CollectCase(ctx context.Context, ticket domain.Ticket, messages []domain.CaseMessage) ([]domain.Evidence, error) {
	selected, status := c.selector(ticket, messages)
	resources := make([]string, 0, len(c.catalog))
	for name := range c.catalog {
		resources = append(resources, name)
	}
	sort.Strings(resources)
	contextData := map[string]any{"selector_field": c.field, "resource": selected, "status": status}
	if status == "clarification_required" {
		contextData["clarification_requirements"] = "Ask for the exact configured resource name: " + strings.Join(resources, ", ")
	}
	if status == "operator_intervention_required" {
		contextData["escalation_requirements"] = "An operator must correct the invalid selector or configure an evidence source; do not repeatedly ask the requester to fix explicit ticket fields."
	}
	result := []domain.Evidence{c.evidence("research.request_context", fmt.Sprintf("%s evidence selection: %s", c.field, status), contextData)}
	if status != "ready" {
		return result, nil
	}
	for _, source := range c.catalog[selected] {
		if err := ctx.Err(); err != nil {
			result = append(result, c.evidence("research.document_unavailable", source.Title+" exceeded the collection deadline", map[string]any{"title": source.Title, "url": source.URL, "status": "deadline_exceeded"}))
			continue
		}
		data, contentType, modified, err := c.read(ctx, source)
		if err != nil {
			// Preserve successful sources and a bounded, sanitized gap observation.
			result = append(result, c.evidence("research.document_unavailable", source.Title+" could not be collected", map[string]any{"title": source.Title, "url": source.URL, "status": "unavailable", "unknown": "Source unavailable; no claims from this document are verified."}))
			continue
		}
		content := string(data)
		if strings.Contains(contentType, "html") {
			content = extractHTML(data)
		}
		if !utf8.ValidString(content) || strings.ContainsRune(content, 0) || strings.TrimSpace(content) == "" {
			result = append(result, c.evidence("research.document_unavailable", source.Title+" is not readable text", map[string]any{"title": source.Title, "status": "unreadable"}))
			continue
		}
		truncated := len(content) > MaxContent
		if truncated {
			content = content[:MaxContent]
			for !utf8.ValidString(content) {
				content = content[:len(content)-1]
			}
		}
		digest := sha256.Sum256(data)
		kind, claimType := "research.vendor_document", "vendor_claim"
		if c.capability == KnowledgeCollect {
			kind, claimType = "research.knowledge_document", "administrator_document"
		}
		result = append(result, c.evidence(kind, source.Title, map[string]any{"title": source.Title, "url": source.URL, "content": content, "content_sha256": hex.EncodeToString(digest[:]), "last_modified": modified, "truncated": truncated, "claim_type": claimType, "resource": selected}))
	}
	return result, nil
}

func (c *Collector) selector(ticket domain.Ticket, messages []domain.CaseMessage) (string, string) {
	values, present := ticket.Metadata.Fields[c.field]
	for key := range ticket.Metadata.Fields {
		if key != c.field && strings.EqualFold(strings.TrimSpace(key), c.field) {
			return "", "operator_intervention_required"
		}
	}
	if present {
		if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] {
			return "", "operator_intervention_required"
		}
		name := strings.ToLower(values[0])
		if _, ok := c.catalog[name]; !ok {
			return "", "operator_intervention_required"
		}
		return name, "ready"
	}
	// Only the latest authenticated requester reply may supply a selector.
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Direction != domain.MessageInbound || m.Source != ticket.Source || m.Author != ticket.ReporterEmail || m.Author == "" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(m.Body))
		if _, ok := c.catalog[name]; ok {
			return name, "ready"
		}
		break
	}
	return "", "clarification_required"
}

func (c *Collector) evidence(kind, summary string, data any) domain.Evidence {
	encoded, _ := json.Marshal(data)
	return domain.Evidence{ID: domain.NewEvidenceID(), Source: Name, Capability: c.capability, Kind: kind, Summary: summary, Data: encoded, ObservedAt: time.Now().UTC()}
}

func (c *Collector) read(ctx context.Context, source Source) ([]byte, string, string, error) {
	var reader io.ReadCloser
	contentType, modified := "text/plain", ""
	if source.URL != "" {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
		if err != nil {
			return nil, "", "", err
		}
		r.Header.Set("Accept", "text/html, text/plain, text/markdown")
		r.Header.Set("User-Agent", "Wardstone/0.1 (bounded vendor evidence collector)")
		response, err := c.client.Do(r)
		if err != nil {
			return nil, "", "", err
		}
		reader = response.Body
		contentType, modified = response.Header.Get("Content-Type"), response.Header.Get("Last-Modified")
		if response.StatusCode != http.StatusOK {
			reader.Close()
			return nil, "", "", errors.New("source unavailable")
		}
		if !strings.HasPrefix(contentType, "text/") {
			reader.Close()
			return nil, "", "", errors.New("source must be text")
		}
	} else {
		stat, err := os.Stat(source.Path)
		if err != nil || !stat.Mode().IsRegular() {
			return nil, "", "", errors.New("source must be a regular file")
		}
		file, err := os.Open(source.Path)
		if err != nil {
			return nil, "", "", err
		}
		stat, err = file.Stat()
		if err != nil || !stat.Mode().IsRegular() {
			file.Close()
			return nil, "", "", errors.New("source must be a regular file")
		}
		reader = file
		modified = stat.ModTime().UTC().Format(time.RFC3339)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, MaxDownload+1))
	if err != nil || len(data) > MaxDownload {
		return nil, "", "", errors.New("source exceeds document budget or could not be read")
	}
	return data, contentType, modified, nil
}

func extractHTML(data []byte) string {
	root, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return ""
	}
	// Prefer the actual document body over site-wide menus and marketing chrome.
	var main *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "main" && main == nil {
			main = n
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			find(child)
		}
	}
	find(root)
	if main != nil {
		root = main
	}
	var out strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "header", "footer", "aside", "noscript":
				return
			}
		}
		if n.Type == html.TextNode {
			text := strings.Join(strings.Fields(n.Data), " ")
			if text != "" {
				out.WriteString(text)
				out.WriteByte('\n')
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return out.String()
}
