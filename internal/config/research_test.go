package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredVendorAndKnowledgeFilePaths(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "wardstone.yaml")
	content := string(base) + `
research:
  vendors:
    Acme:
      - title: Independent report
        path: evidence/audit.pdf
        claim_type: independent_report
      - title: Vendor questionnaire
        path: evidence/questionnaire.md
  knowledge:
    VPN:
      - title: VPN manual
        path: evidence/vpn.pdf
`
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := loadPolicy(file, &c); err != nil {
		t.Fatal(err)
	}
	if c.VendorSources["acme"][0].Path != filepath.Join(dir, "evidence/audit.pdf") || c.VendorSources["acme"][0].ClaimType != "independent_report" || c.KnowledgeSources["vpn"][0].Path != filepath.Join(dir, "evidence/vpn.pdf") {
		t.Fatalf("file catalogs: %+v %+v", c.VendorSources, c.KnowledgeSources)
	}
}
