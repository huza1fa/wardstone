package config

import (
	"testing"

	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/policy"
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
}

func TestLoadPolicyRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	var config Config
	if err := loadPolicy("testdata/invalid.yaml", &config); err == nil {
		t.Fatal("expected invalid policy mode to be rejected")
	}
}
