//go:build linux

package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestConfiguredPDFCollection(t *testing.T) {
	for _, tc := range []struct {
		file, status string
		pages        int
		truncated    bool
	}{
		{"readable.pdf", "", 2, false},
		{"encrypted.pdf", "pdf_encrypted", 0, false},
		{"scanned.pdf", "pdf_pages_without_text", 1, false},
		{"over-pages.pdf", "pdf_page_limit", 0, false},
		{"dense.pdf", "pdf_excerpt_limit", 1, true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			path, _ := filepath.Abs(filepath.Join("testdata", tc.file))
			collector, err := NewCollector(VendorCollect, Catalog{"Acme": {{Title: "Synthetic report", Path: path, ClaimType: "independent_report"}}}, http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			items, err := collector.Collect(context.Background(), domain.Ticket{Metadata: domain.TicketMetadata{Fields: map[string][]string{"Vendor": {"Acme"}}}})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			sum := sha256.Sum256(raw)
			pages, status, total := 0, "", 0
			for _, item := range items[1:] {
				var d struct {
					Content   string
					Digest    string `json:"content_sha256"`
					Page      int    `json:"page_number"`
					Pages     int    `json:"page_count"`
					Claim     string `json:"claim_type"`
					Status    string
					Truncated bool
				}
				if err := json.Unmarshal(item.Data, &d); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(item.Data), path) || d.Digest != hex.EncodeToString(sum[:]) {
					t.Fatalf("leaked path or wrong snapshot: %s", item.Data)
				}
				metadata := audit.EvidenceData(item)
				if strings.Contains(string(audit.Data(metadata)), path) || metadata["content"] != nil {
					t.Fatal("audit leaked file body/path")
				}
				if item.Kind == "research.document_unavailable" {
					status = d.Status
					continue
				}
				pages++
				total += len(d.Content)
				if d.Page != pages || d.Pages < d.Page || d.Claim != "independent_report" || d.Truncated != tc.truncated || metadata["page_number"] != d.Page {
					t.Fatalf("page provenance: %s", item.Data)
				}
				if tc.file == "readable.pdf" && !strings.Contains(d.Content, []string{"Identity controls", "Independent assurance"}[pages-1]) {
					t.Fatalf("wrong page content: %s", item.Data)
				}
			}
			if pages != tc.pages || status != tc.status || total > MaxContent {
				t.Fatalf("pages=%d status=%q bytes=%d items=%+v", pages, status, total, items)
			}
		})
	}
}

func TestPDFRejectsMalformedAndOversizedFiles(t *testing.T) {
	for _, content := range [][]byte{[]byte("not a PDF"), []byte("%PDF-1.7\nmalformed"), make([]byte, MaxPDFDownload+1)} {
		file := filepath.Join(t.TempDir(), "bad.pdf")
		if err := os.WriteFile(file, content, 0600); err != nil {
			t.Fatal(err)
		}
		collector, err := NewCollector(KnowledgeCollect, Catalog{"vpn": {{Title: "Bad report", Path: file}}}, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		items, err := collector.Collect(context.Background(), domain.Ticket{Metadata: domain.TicketMetadata{Fields: map[string][]string{"Knowledge topic": {"vpn"}}}})
		if err != nil || len(items) != 2 || items[1].Kind != "research.document_unavailable" {
			t.Fatalf("malformed accepted: %+v %v", items, err)
		}
	}
}

func TestConfinedFilesRejectEscapingSymlinkAndFIFO(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("private content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(base, "fifo.txt"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link.txt", "fifo.txt"} {
		collector, err := NewCollector(VendorCollect, Catalog{"acme": {{Title: "Configured file", Path: filepath.Join(base, name)}}}, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		items, err := collector.Collect(context.Background(), domain.Ticket{Metadata: domain.TicketMetadata{Fields: map[string][]string{"Vendor": {"acme"}}}})
		if err != nil || len(items) != 2 || items[1].Kind != "research.document_unavailable" || strings.Contains(string(items[1].Data), "private content") {
			t.Fatalf("unsafe file accepted: %+v %v", items, err)
		}
	}
}

func TestPDFProcessBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, status := runPDFCommand(ctx, []byte("%PDF-"), "pdfinfo", "-"); status != "pdf_timeout" {
		t.Fatal(status)
	}
	out := &cappedPDFOutput{limit: 4}
	if _, err := out.Write([]byte("12345")); err == nil || !out.exceeded || out.Len() != 0 {
		t.Fatal("unbounded output")
	}
	// Exercise the production process supervisor, not a fake timeout result.
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, status := runPDFCommand(ctx, nil, "sleep", "10"); status != "pdf_timeout" || time.Since(start) > time.Second {
		t.Fatalf("process timeout status=%s elapsed=%s", status, time.Since(start))
	}
	// Parser processes cannot inherit server credentials or read the home tree.
	t.Setenv("WARDSTONE_OPERATOR_TOKEN", "synthetic-do-not-inherit")
	data, status := runPDFCommand(context.Background(), nil, "env")
	if status != "" || strings.Contains(string(data), "WARDSTONE_OPERATOR_TOKEN") || strings.Contains(string(data), "synthetic-do-not-inherit") {
		t.Fatalf("parser environment: %q %s", data, status)
	}
	if _, status := runPDFCommand(context.Background(), nil, "cat", "/etc/passwd"); status == "" {
		t.Fatal("sandbox exposed host filesystem")
	}
	if _, status := runPDFCommand(context.Background(), []byte("123456"), "head", "-c", "300000", "/dev/zero"); status != "pdf_output_limit" {
		t.Fatalf("output cap=%s", status)
	}
}

func TestPDFExtractionPageAndDeadlineContracts(t *testing.T) {
	for _, tc := range []struct{ info, text, status string }{
		{"Pages: 1\nEncrypted: yes", "", "pdf_encrypted"},
		{"Pages: 21", "", "pdf_page_limit"},
		{"Pages: 2", "one page\f", "pdf_malformed"},
		{"Pages: 1", "\xff\f", "pdf_unreadable"},
	} {
		calls := 0
		result := extractPDF(context.Background(), []byte("%PDF-1.7"), func(ctx context.Context, data []byte, tool string, args ...string) ([]byte, string) {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("missing deadline")
			}
			if calls == 1 {
				return []byte(tc.info), ""
			}
			return []byte(tc.text), ""
		})
		if result.status != tc.status {
			t.Fatalf("status=%s want=%s", result.status, tc.status)
		}
	}
}
