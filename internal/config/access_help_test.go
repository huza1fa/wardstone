package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wardstone-project/wardstone/internal/connectors/google"
)

func TestAccessHelpOptionalAndDefensiveCopies(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"", "access_help: {}\n", "access_help:\n  resources: {}\n"} {
		var config Config
		if err := loadPolicy(writeRoutingConfig(t, suffix), &config); err != nil {
			t.Fatal(err)
		}
		if len(config.AccessHelp.Resources()) != 0 {
			t.Fatal("omitted resources should be empty")
		}
	}
	var config Config
	if err := loadPolicy(writeRoutingConfig(t, "access_help:\n  resources:\n    ' Engineering tools ': TOOLS@EXAMPLE.COM\n"), &config); err != nil {
		t.Fatal(err)
	}
	copy := config.AccessHelp.Resources()
	if copy["Engineering tools"] != "tools@example.com" {
		t.Fatalf("resources=%v", copy)
	}
	copy["Engineering tools"] = "changed@example.com"
	if config.AccessHelp.Resources()["Engineering tools"] != "tools@example.com" {
		t.Fatal("resources leaked mutable map")
	}
}

func TestAccessHelpRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for name, resources := range map[string]string{
		"normalized duplicate":    "    Engineering tools: tools@example.com\n    ' engineering TOOLS ': other@example.com\n",
		"exact duplicate":         "    Tools: tools@example.com\n    Tools: other@example.com\n",
		"Unicode fold duplicate":  "    Σ: tools@example.com\n    ς: other@example.com\n",
		"blank name":              "    '   ': tools@example.com\n",
		"long name":               "    '" + strings.Repeat("x", 129) + "': tools@example.com\n",
		"control name":            "    \"Tools\\nMore\": tools@example.com\n",
		"display group":           "    Tools: 'Tools <tools@example.com>'\n",
		"group query injection":   "    Tools: 'tools@example.com?admin=true'\n",
		"group path injection":    "    Tools: '../groups/tools@example.com'\n",
		"group percent injection": "    Tools: 'tools%2fadmin@example.com'\n",
		"group URL":               "    Tools: 'https://example.com/group'\n",
		"blank group":             "    Tools: ''\n",
		"malformed group":         "    Tools: 'tools@-example.com'\n",
	} {
		t.Run(name, func(t *testing.T) {
			var config Config
			if err := loadPolicy(writeRoutingConfig(t, "access_help:\n  resources:\n"+resources), &config); err == nil {
				t.Fatal("expected invalid resource rejection")
			}
		})
	}
	var config Config
	if err := loadPolicy(writeRoutingConfig(t, "access_help:\n  endpoint: https://example.com\n"), &config); err == nil {
		t.Fatal("accepted arbitrary endpoint")
	}
	if _, err := google.ValidateAccessResources(map[string]string{string([]byte{0xff}): "tools@example.com"}); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
}

func TestAccessHelpResourceCountBound(t *testing.T) {
	t.Parallel()
	var resources strings.Builder
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&resources, "    Resource %d: group%d@example.com\n", i, i)
	}
	var config Config
	if err := loadPolicy(writeRoutingConfig(t, "access_help:\n  resources:\n"+resources.String()), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.AccessHelp.Resources()) != 128 {
		t.Fatal("expected 128 resources")
	}
	resources.WriteString("    Extra: extra@example.com\n")
	if err := loadPolicy(writeRoutingConfig(t, "access_help:\n  resources:\n"+resources.String()), &config); err == nil {
		t.Fatal("accepted 129 resources")
	}
}
