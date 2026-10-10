package config

import (
	"testing"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestOpenRouterShortcutAndReviewMode(t *testing.T) {
	clearRuntimeEnvironment(t)
	t.Setenv("WARDSTONE_CONFIG_FILE", "testdata/valid.yaml")
	t.Setenv("WARDSTONE_DATABASE_URL", "postgres://example.test/wardstone")
	t.Setenv("WARDSTONE_OPERATOR_TOKEN", "operator")
	t.Setenv("WARDSTONE_AI_KEY", "openrouter-test-key")
	t.Setenv("WARDSTONE_MODE", "REVIEW")
	c, err := Load()
	if err != nil || c.Mode != domain.OperatingModeReview || c.OpenAIBaseURL != "https://openrouter.ai/api/v1" || c.OpenAIModel != "openai/gpt-4.1-mini" || c.OpenAIAPIKey != "openrouter-test-key" {
		t.Fatalf("shortcut not configured: err=%v", err)
	}
	t.Setenv("WARDSTONE_OPENAI_BASE_URL", "https://other.example/v1")
	if _, err := Load(); err == nil {
		t.Fatal("OpenRouter key could leak to another provider")
	}
	t.Setenv("WARDSTONE_OPENAI_API_KEY", "explicit-key")
	t.Setenv("WARDSTONE_OPENAI_MODEL", "explicit-model")
	c, err = Load()
	if err != nil || c.OpenAIAPIKey != "explicit-key" || c.OpenAIBaseURL != "https://other.example/v1" {
		t.Fatal("explicit configuration was not respected")
	}
}
