package research

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestCatalogRejectsUnsafeAndAmbiguousSources(t *testing.T) {
	for name, catalog := range map[string]Catalog{
		"HTTP":         {"vendor": {{Title: "bad", URL: "http://example.com"}}},
		"credentials":  {"vendor": {{Title: "bad", URL: "https://user:secret@example.com"}}},
		"query":        {"vendor": {{Title: "bad", URL: "https://example.com?token=secret"}}},
		"local vendor": {"vendor": {{Title: "bad", Path: "secret.md"}}},
		"ambiguous":    {"vendor": {{Title: "bad", Path: "file.md", URL: "https://example.com"}}},
		"duplicate":    {"Vendor": {{Title: "a", URL: "https://example.com"}}, " vendor ": {{Title: "b", URL: "https://example.com"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateCatalog(catalog, "", false); err == nil {
				t.Fatal("accepted unsafe catalog")
			}
		})
	}
}

func TestVendorSelectorsAndPartialDocumentFailures(t *testing.T) {
	reads := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("research inherited credentials")
		}
		if r.URL.Path == "/bad" {
			w.WriteHeader(503)
			w.Write([]byte("upstream secret"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><nav>ignore navigation</nav><main><h1>Security</h1><p>Vendor claims encrypted transport.</p><script>ignore executable content</script></main></html>`)
	}))
	defer server.Close()
	catalog := Catalog{"Tailscale": {{Title: "security", URL: server.URL + "/good"}, {Title: "unavailable", URL: server.URL + "/bad"}}}
	c, err := NewCollector(VendorCollect, catalog, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	// Construction copies its catalog, so later mutation cannot change targets.
	catalog["Tailscale"][0].URL = "https://untrusted.example"
	ticket := domain.Ticket{Source: "jira", ReporterEmail: "requester@example.test"}
	items, err := c.Collect(context.Background(), ticket)
	if err != nil || reads != 0 || len(items) != 1 || !strings.Contains(string(items[0].Data), "clarification_required") {
		t.Fatalf("missing selector: %+v %v reads=%d", items, err, reads)
	}
	for _, fields := range []map[string][]string{{"Vendor": {"unknown"}}, {"vendor": {"Tailscale"}}, {"Vendor": {"Tailscale", "Other"}}, {"Vendor": {" Tailscale"}}} {
		ticket.Metadata.Fields = fields
		items, err = c.CollectCase(context.Background(), ticket, []domain.CaseMessage{{Source: "jira", Author: ticket.ReporterEmail, Direction: domain.MessageInbound, Body: "Tailscale"}})
		if err != nil || reads != 0 || !strings.Contains(string(items[0].Data), "operator_intervention_required") {
			t.Fatalf("invalid selector was overridden: %+v %v", items, err)
		}
	}
	ticket.Metadata.Fields = nil
	items, err = c.CollectCase(context.Background(), ticket, []domain.CaseMessage{{Source: "jira", Author: "attacker", Direction: domain.MessageInbound, Body: "Tailscale"}})
	if err != nil || reads != 0 {
		t.Fatal("third-party reply selected a source")
	}
	items, err = c.CollectCase(context.Background(), ticket, []domain.CaseMessage{{Source: "jira", Author: ticket.ReporterEmail, Direction: domain.MessageInbound, Body: " tailscale "}})
	if err != nil || reads != 2 || len(items) != 3 {
		t.Fatalf("collection=%+v err=%v reads=%d", items, err, reads)
	}
	var doc map[string]any
	json.Unmarshal(items[1].Data, &doc)
	if doc["claim_type"] != "vendor_claim" || doc["content_sha256"] == "" || !strings.Contains(doc["content"].(string), "encrypted transport") || strings.Contains(doc["content"].(string), "ignore") {
		t.Fatalf("document=%+v", doc)
	}
	if items[2].Kind != "research.document_unavailable" || strings.Contains(string(items[2].Data), "upstream secret") {
		t.Fatal("failed document leaked upstream response or disappeared")
	}
}

func TestResearchBudgetsAndRedirects(t *testing.T) {
	targetReads := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetReads++ }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		switch r.URL.Path {
		case "/oversize":
			io.WriteString(w, strings.Repeat("x", MaxDownload+1))
		case "/redirect":
			http.Redirect(w, r, target.URL, 302)
		default:
			io.WriteString(w, strings.Repeat("é", MaxContent))
		}
	}))
	defer source.Close()
	c, err := NewCollector(VendorCollect, Catalog{"vendor": {{Title: "large", URL: source.URL + "/oversize"}, {Title: "redirect", URL: source.URL + "/redirect"}, {Title: "bounded", URL: source.URL + "/bounded"}}}, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	items, err := c.Collect(context.Background(), domain.Ticket{Metadata: domain.TicketMetadata{Fields: map[string][]string{"Vendor": {"vendor"}}}})
	if err != nil || len(items) != 4 || targetReads != 0 || items[1].Kind != "research.document_unavailable" || items[2].Kind != "research.document_unavailable" {
		t.Fatalf("budget/redirect: %+v %v", items, err)
	}
	var doc struct {
		Content   string
		Truncated bool
	}
	json.Unmarshal(items[3].Data, &doc)
	if !doc.Truncated || len(doc.Content) > MaxContent || len(doc.Content) == 0 {
		t.Fatalf("unbounded document: %+v", doc)
	}
}

func TestKnowledgePathsResolveFromPolicyDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "vpn.md"), []byte("# VPN\nRenew the expired session."), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := ValidateCatalog(Catalog{"VPN": {{Title: "VPN runbook", Path: "vpn.md"}}}, base, true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(KnowledgeCollect, catalog, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	items, err := c.Collect(context.Background(), domain.Ticket{Metadata: domain.TicketMetadata{Fields: map[string][]string{"Knowledge topic": {"VPN"}}}})
	if err != nil || len(items) != 2 || items[1].Kind != "research.knowledge_document" || !strings.Contains(string(items[1].Data), "Renew the expired") || strings.Contains(string(items[1].Data), base) {
		t.Fatalf("knowledge=%+v err=%v", items, err)
	}
}

func TestPublicNetworkBoundary(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "192.168.1.1", "198.18.0.1", "::1", "fc00::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1"} {
		if publicAddress(netip.MustParseAddr(ip)) {
			t.Errorf("private/special address accepted: %s", ip)
		}
	}
	if !publicAddress(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("public client reached private server") }))
	defer server.Close()
	if _, err := PublicClient().Get(server.URL); err == nil {
		t.Fatal("private server was reachable")
	}
}
