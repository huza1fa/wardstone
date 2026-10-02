package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/policy"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

func TestLoadPolicy(t *testing.T) {
	t.Parallel()
	var config Config
	if err := loadPolicy("testdata/valid.yaml", &config); err != nil {
		t.Fatal(err)
	}
	if got := config.PolicyRules[domain.CapabilityName("google.users.get")]; got != policy.RuleAllow {
		t.Fatalf("read rule = %q, want allow", got)
	}
	if got := config.PolicyRules[domain.CapabilityName("google.groups.add_member")]; got != policy.RuleApproval {
		t.Fatalf("mutation rule = %q, want approval", got)
	}
	if len(config.Specialists) != 2 || config.Specialists[0].Name != "help_desk" {
		t.Fatalf("specialists = %+v", config.Specialists)
	}
	if len(config.RoutingRules) != 1 || config.RoutingRules[0].Specialist != "access_management" {
		t.Fatalf("routing rules = %+v", config.RoutingRules)
	}
}

func TestLoadAllowsUnconfiguredConnectors(t *testing.T) {
	clearRuntimeEnvironment(t)
	t.Setenv("WARDSTONE_CONFIG_FILE", "testdata/valid.yaml")
	t.Setenv("WARDSTONE_DATABASE_URL", "postgres://example.test/wardstone")
	t.Setenv("WARDSTONE_OPERATOR_TOKEN", "operator-secret")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.JiraInboundConfigured() || config.JiraConfigured() || config.GoogleConfigured() || config.ModelConfigured() {
		t.Fatalf("unexpected configured connectors: %+v", config)
	}
}

func TestLoadRejectsPartialConnectorConfiguration(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"Jira base only":          {"WARDSTONE_JIRA_BASE_URL": "https://example.atlassian.net"},
		"Jira missing token":      {"WARDSTONE_JIRA_BASE_URL": "https://example.atlassian.net", "WARDSTONE_JIRA_EMAIL": "bot@example.test"},
		"model key without model": {"WARDSTONE_OPENAI_API_KEY": "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			clearRuntimeEnvironment(t)
			t.Setenv("WARDSTONE_CONFIG_FILE", "testdata/valid.yaml")
			t.Setenv("WARDSTONE_DATABASE_URL", "postgres://example.test/wardstone")
			t.Setenv("WARDSTONE_OPERATOR_TOKEN", "operator-secret")
			for key, value := range values {
				t.Setenv(key, value)
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected partial configuration to be rejected")
			}
		})
	}
}

func TestLoadReportsConfiguredConnectorGroups(t *testing.T) {
	clearRuntimeEnvironment(t)
	for key, value := range map[string]string{
		"WARDSTONE_CONFIG_FILE":         "testdata/valid.yaml",
		"WARDSTONE_DATABASE_URL":        "postgres://example.test/wardstone",
		"WARDSTONE_OPERATOR_TOKEN":      "operator-secret",
		"WARDSTONE_JIRA_WEBHOOK_SECRET": "webhook-secret",
		"WARDSTONE_JIRA_BASE_URL":       "https://example.atlassian.net",
		"WARDSTONE_JIRA_EMAIL":          "bot@example.test",
		"WARDSTONE_JIRA_API_TOKEN":      "jira-secret",
		"WARDSTONE_GOOGLE_ACCESS_TOKEN": "google-secret",
		"WARDSTONE_OPENAI_MODEL":        "test-model",
	} {
		t.Setenv(key, value)
	}
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !config.JiraInboundConfigured() || !config.JiraConfigured() || !config.GoogleConfigured() || !config.ModelConfigured() {
		t.Fatalf("configured groups not detected: %+v", config)
	}
}

func clearRuntimeEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"WARDSTONE_CONFIG_FILE", "WARDSTONE_DATABASE_URL", "WARDSTONE_OPERATOR_TOKEN", "WARDSTONE_MODE",
		"WARDSTONE_JIRA_WEBHOOK_SECRET", "WARDSTONE_JIRA_BASE_URL", "WARDSTONE_JIRA_EMAIL", "WARDSTONE_JIRA_API_TOKEN",
		"WARDSTONE_GOOGLE_ACCESS_TOKEN", "WARDSTONE_OPENAI_BASE_URL", "WARDSTONE_OPENAI_API_KEY", "WARDSTONE_OPENAI_MODEL",
	} {
		t.Setenv(key, "")
	}
}

func TestIntentConfigurationDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		routing string
		want    specialists.IntentConfig
	}{
		{"omitted", "", specialists.DefaultIntentConfig()},
		{"disabled", "routing:\n  intent:\n    enabled: false\n", specialists.IntentConfig{Enabled: false, MinConfidence: 0.75, Timeout: 10 * time.Second}},
		{"partial", "routing:\n  intent:\n    min_confidence: 0.9\n", specialists.IntentConfig{Enabled: true, MinConfidence: 0.9, Timeout: 10 * time.Second}},
		{"full", "routing:\n  intent:\n    enabled: true\n    min_confidence: 1\n    timeout: 2s\n", specialists.IntentConfig{Enabled: true, MinConfidence: 1, Timeout: 2 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var config Config
			if err := loadPolicy(writeRoutingConfig(t, tc.routing), &config); err != nil {
				t.Fatal(err)
			}
			if config.IntentClassification != tc.want {
				t.Fatalf("intent = %+v, want %+v", config.IntentClassification, tc.want)
			}
		})
	}
}

func TestRoutingConfigRejectsMalformedSettings(t *testing.T) {
	t.Parallel()
	for name, setting := range map[string]string{
		"unknown intent option":  "intent:\n    enable: false",
		"confidence zero":        "intent:\n    min_confidence: 0",
		"confidence high":        "intent:\n    min_confidence: 1.01",
		"confidence nan":         "intent:\n    min_confidence: .nan",
		"confidence infinite":    "intent:\n    min_confidence: .inf",
		"timeout zero":           "intent:\n    timeout: 0s",
		"timeout long":           "intent:\n    timeout: 6m",
		"timeout malformed":      "intent:\n    timeout: tomorrow",
		"unknown routing option": "unknown: true",
	} {
		t.Run(name, func(t *testing.T) {
			var config Config
			if err := loadPolicy(writeRoutingConfig(t, "routing:\n  "+setting+"\n"), &config); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	for name, match := range map[string]string{
		"empty selector":     "request_types: []",
		"null selector":      "request_types: null",
		"empty fields":       "fields: {}",
		"null fields":        "fields: null",
		"unknown selector":   "request_type: [Access request]",
		"duplicate field":    "fields:\n          key: [A]\n          ' key ': [B]",
		"empty field values": "fields:\n          key: []",
	} {
		t.Run(name, func(t *testing.T) {
			routing := "routing:\n  rules:\n    - name: test\n      specialist: help_desk\n      classification: triage\n      match:\n        " + match + "\n"
			var config Config
			if err := loadPolicy(writeRoutingConfig(t, routing), &config); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestRoutingConfigSources(t *testing.T) {
	t.Parallel()
	routing := "routing:\n  rules:\n    - name: jira-test\n      specialist: help_desk\n      classification: triage\n      match:\n        sources: [jira]\n"
	var config Config
	if err := loadPolicy(writeRoutingConfig(t, routing), &config); err != nil {
		t.Fatal(err)
	}
	if got := config.RoutingRules[0].Match.Sources; len(got) != 1 || got[0] != "jira" {
		t.Fatalf("sources = %v", got)
	}
}

func TestRoutingConfigRejectsOversizedAndMultipleDocuments(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"---\nrouting: {}\n", "#" + strings.Repeat("x", 1<<20)} {
		var config Config
		if err := loadPolicy(writeRoutingConfig(t, suffix), &config); err == nil {
			t.Fatal("expected rejection")
		}
	}
}

func writeRoutingConfig(t *testing.T, routing string) string {
	t.Helper()
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Split(string(base), "\nrouting:")[0] + "\n" + routing
	path := filepath.Join(t.TempDir(), "wardstone.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPolicyRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	var config Config
	if err := loadPolicy("testdata/invalid.yaml", &config); err == nil {
		t.Fatal("expected invalid policy mode to be rejected")
	}
}

func TestLoadPolicyRejectsUnknownSpecialistHandoff(t *testing.T) {
	t.Parallel()
	var config Config
	if err := loadPolicy("testdata/invalid_specialist.yaml", &config); err == nil {
		t.Fatal("expected invalid specialist handoff to be rejected")
	}
}
