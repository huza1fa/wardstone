package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/policy"
	"github.com/wardstone-project/wardstone/internal/specialists"
)

type Config struct {
	DatabaseURL          string
	ListenAddress        string
	Mode                 domain.OperatingMode
	Workers              int
	MaxCollectors        int
	CollectorTimeout     time.Duration
	ModelTimeout         time.Duration
	JobLease             time.Duration
	ApprovalLifetime     time.Duration
	MaxHandoffs          int
	MaxFollowUpQuestions int
	JiraWebhookSecret    string
	JiraBaseURL          string
	JiraEmail            string
	JiraAPIToken         string
	OperatorToken        string
	GoogleBaseURL        string
	GoogleAccessToken    string
	OpenAIBaseURL        string
	OpenAIAPIKey         string
	OpenAIModel          string
	PolicyRules          map[domain.CapabilityName]policy.RuleMode
	Specialists          []specialists.Profile
	RoutingRules         []specialists.RouteRule
	IntentClassification specialists.IntentConfig
}

func Load() (Config, error) {
	policyPath := os.Getenv("WARDSTONE_CONFIG_FILE")
	if policyPath == "" {
		return Config{}, errors.New("WARDSTONE_CONFIG_FILE is required")
	}
	config := Config{
		DatabaseURL:       os.Getenv("WARDSTONE_DATABASE_URL"),
		ListenAddress:     valueOrDefault("WARDSTONE_LISTEN_ADDRESS", "127.0.0.1:8080"),
		Mode:              domain.OperatingMode(valueOrDefault("WARDSTONE_MODE", string(domain.OperatingModeShadow))),
		JiraWebhookSecret: os.Getenv("WARDSTONE_JIRA_WEBHOOK_SECRET"),
		JiraBaseURL:       os.Getenv("WARDSTONE_JIRA_BASE_URL"),
		JiraEmail:         os.Getenv("WARDSTONE_JIRA_EMAIL"),
		JiraAPIToken:      os.Getenv("WARDSTONE_JIRA_API_TOKEN"),
		OperatorToken:     os.Getenv("WARDSTONE_OPERATOR_TOKEN"),
		GoogleBaseURL:     valueOrDefault("WARDSTONE_GOOGLE_BASE_URL", "https://admin.googleapis.com/admin/directory/v1"),
		GoogleAccessToken: os.Getenv("WARDSTONE_GOOGLE_ACCESS_TOKEN"),
		OpenAIBaseURL:     valueOrDefault("WARDSTONE_OPENAI_BASE_URL", "https://api.openai.com/v1"),
		OpenAIAPIKey:      os.Getenv("WARDSTONE_OPENAI_API_KEY"),
		OpenAIModel:       os.Getenv("WARDSTONE_OPENAI_MODEL"),
	}
	if err := loadPolicy(policyPath, &config); err != nil {
		return Config{}, err
	}
	var err error
	if config.Workers, err = intValue("WARDSTONE_WORKERS", 4); err != nil {
		return Config{}, err
	}
	if config.MaxCollectors, err = intValue("WARDSTONE_MAX_COLLECTORS", 4); err != nil {
		return Config{}, err
	}
	if config.CollectorTimeout, err = durationValue("WARDSTONE_COLLECTOR_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if config.ModelTimeout, err = durationValue("WARDSTONE_MODEL_TIMEOUT", 60*time.Second); err != nil {
		return Config{}, err
	}
	if config.JobLease, err = durationValue("WARDSTONE_JOB_LEASE", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if config.ApprovalLifetime, err = durationValue("WARDSTONE_APPROVAL_LIFETIME", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if config.MaxHandoffs, err = intValue("WARDSTONE_MAX_HANDOFFS", 3); err != nil {
		return Config{}, err
	}
	if config.MaxFollowUpQuestions, err = intValue("WARDSTONE_MAX_FOLLOW_UP_QUESTIONS", 3); err != nil {
		return Config{}, err
	}
	if config.Mode != domain.OperatingModeShadow {
		return Config{}, fmt.Errorf("only SHADOW mode is implemented; got %s", config.Mode)
	}
	if config.DatabaseURL == "" || config.OperatorToken == "" {
		return Config{}, errors.New("WARDSTONE_DATABASE_URL and WARDSTONE_OPERATOR_TOKEN are required")
	}
	if configured := countConfigured(config.JiraBaseURL, config.JiraEmail, config.JiraAPIToken); configured != 0 && configured != 3 {
		return Config{}, errors.New("WARDSTONE_JIRA_BASE_URL, WARDSTONE_JIRA_EMAIL, and WARDSTONE_JIRA_API_TOKEN must be configured together")
	}
	if config.OpenAIModel == "" && config.OpenAIAPIKey != "" {
		return Config{}, errors.New("WARDSTONE_OPENAI_MODEL is required when WARDSTONE_OPENAI_API_KEY is configured")
	}
	if config.Workers < 1 || config.MaxCollectors < 1 || config.MaxHandoffs < 1 || config.MaxFollowUpQuestions < 1 {
		return Config{}, errors.New("worker, collector, handoff, and follow-up question limits must be positive")
	}
	return config, nil
}

func (c Config) JiraInboundConfigured() bool { return c.JiraWebhookSecret != "" }

func (c Config) JiraConfigured() bool {
	return c.JiraBaseURL != "" && c.JiraEmail != "" && c.JiraAPIToken != ""
}

func (c Config) GoogleConfigured() bool { return c.GoogleAccessToken != "" }

func (c Config) ModelConfigured() bool { return c.OpenAIModel != "" }

func countConfigured(values ...string) int {
	configured := 0
	for _, value := range values {
		if value != "" {
			configured++
		}
	}
	return configured
}

func loadPolicy(path string, config *Config) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open configuration file: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return fmt.Errorf("read configuration file: %w", err)
	}
	if len(contents) > 1<<20 {
		return errors.New("configuration file must be at most 1 MiB")
	}
	var raw struct {
		Capabilities map[domain.CapabilityName]struct {
			Mode policy.RuleMode `yaml:"mode"`
		} `yaml:"capabilities"`
		Specialists []struct {
			Name         domain.SpecialistName   `yaml:"name"`
			Instructions string                  `yaml:"instructions"`
			Collectors   []domain.CapabilityName `yaml:"collectors"`
			Capabilities []domain.CapabilityName `yaml:"capabilities"`
			HandoffTo    []domain.SpecialistName `yaml:"handoff_to"`
		} `yaml:"specialists"`
		Routing struct {
			Rules  []specialists.RouteRule  `yaml:"rules"`
			Intent specialists.IntentConfig `yaml:"intent"`
		} `yaml:"routing"`
	}
	raw.Routing.Intent = specialists.DefaultIntentConfig()
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode configuration file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("configuration file must contain one YAML document")
	}
	if len(raw.Capabilities) == 0 {
		return errors.New("configuration must define at least one capability policy")
	}
	config.PolicyRules = make(map[domain.CapabilityName]policy.RuleMode, len(raw.Capabilities))
	for name, entry := range raw.Capabilities {
		if name == "" {
			return errors.New("capability policy name cannot be empty")
		}
		switch entry.Mode {
		case policy.RuleAllow, policy.RuleApproval, policy.RuleDeny:
			config.PolicyRules[name] = entry.Mode
		default:
			return fmt.Errorf("capability %s has invalid policy mode %q", name, entry.Mode)
		}
	}
	profiles := make([]specialists.Profile, 0, len(raw.Specialists))
	for _, rawProfile := range raw.Specialists {
		profiles = append(profiles, specialists.Profile{
			Name:                rawProfile.Name,
			Instructions:        rawProfile.Instructions,
			AllowedCollectors:   append([]domain.CapabilityName(nil), rawProfile.Collectors...),
			AllowedCapabilities: append([]domain.CapabilityName(nil), rawProfile.Capabilities...),
			HandoffTargets:      append([]domain.SpecialistName(nil), rawProfile.HandoffTo...),
		})
	}
	registry, err := specialists.NewRegistry(profiles...)
	if err != nil {
		return fmt.Errorf("specialists: %w", err)
	}
	if _, err := specialists.NewDispatcherWithRules(registry, raw.Routing.Rules); err != nil {
		return fmt.Errorf("specialists: %w", err)
	}
	if err := raw.Routing.Intent.Validate(); err != nil {
		return fmt.Errorf("routing intent: %w", err)
	}
	config.Specialists = profiles
	config.RoutingRules = append([]specialists.RouteRule(nil), raw.Routing.Rules...)
	config.IntentClassification = raw.Routing.Intent
	return nil
}

func valueOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func intValue(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func durationValue(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return parsed, nil
}
