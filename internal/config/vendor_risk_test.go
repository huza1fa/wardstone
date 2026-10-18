package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rubricYAML = `
vendor_risk:
  rubric:
    version: fixture-v1
    moderate_at: 25
    high_at: 60
    criteria:
      - id: identity
        requirement: Identity controls match deployment scope.
        weight: 40
        evidence: vendor_claim
      - id: assurance
        requirement: Independent assurance covers deployment.
        weight: 60
        evidence: independent_report
  deployments:
    Acme:
      use_case: Internal test
      data_classification: Internal
      access_scope: Read only
`

func TestVendorRiskConfigurationValidation(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, from, to string
		valid          bool
	}{
		{name: "valid", valid: true},
		{name: "weights", from: "weight: 40", to: "weight: 39"},
		{name: "thresholds", from: "high_at: 60", to: "high_at: 25"},
		{name: "unknown evidence", from: "evidence: vendor_claim", to: "evidence: trusted"},
		{name: "duplicate criterion", from: "id: assurance", to: "id: identity"},
		{name: "unknown option", from: "high_at: 60", to: "high_at: 60\n    permissions: admin"},
		{name: "empty version", from: "version: fixture-v1", to: "version: ''"},
		{name: "deployment context can be incomplete", from: "access_scope: Read only", to: "access_scope: ''", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "wardstone.yaml")
			text := string(base) + rubricYAML
			if tc.from != "" {
				text = strings.Replace(text, tc.from, tc.to, 1)
			}
			if err := os.WriteFile(file, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			c := Config{}
			err := loadPolicy(file, &c)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if tc.valid && c.VendorRisk.Deployments["acme"].UseCase != "Internal test" {
				t.Fatal("vendor deployment key was not normalized")
			}
		})
	}
}
