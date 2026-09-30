// Package specialists defines the least-privilege investigation roles used by
// the shared Wardstone runtime. Profiles are configuration, not agents: they
// cannot create capabilities or bypass policy.
package specialists

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const (
	HelpDesk         domain.SpecialistName = "help_desk"
	AccessManagement domain.SpecialistName = "access_management"
	VendorReview     domain.SpecialistName = "vendor_review"
	SystemsKnowledge domain.SpecialistName = "systems_knowledge"
)

// Profile constrains one operational competency. The registry copies all
// inputs so callers cannot widen a profile after service construction.
type Profile struct {
	Name                domain.SpecialistName
	Instructions        string
	AllowedCollectors   []domain.CapabilityName
	AllowedCapabilities []domain.CapabilityName
	HandoffTargets      []domain.SpecialistName
}

func (p Profile) Validate() error {
	if !validConfiguredText(string(p.Name), 64) {
		return errors.New("specialist name must be nonblank valid UTF-8 without NUL and at most 64 bytes")
	}
	if !validConfiguredText(p.Instructions, 8000) {
		return errors.New("specialist instructions must be nonblank valid UTF-8 without NUL and at most 8000 bytes")
	}
	if len(p.AllowedCollectors) > 128 || len(p.AllowedCapabilities) > 128 || len(p.HandoffTargets) > 32 {
		return errors.New("specialist allowlists exceed configured safety limits")
	}
	for _, capability := range append(append([]domain.CapabilityName(nil), p.AllowedCollectors...), p.AllowedCapabilities...) {
		if !validConfiguredText(string(capability), 128) {
			return errors.New("specialist capability names must be nonblank valid UTF-8 without NUL and at most 128 bytes")
		}
	}
	for _, target := range p.HandoffTargets {
		if !validConfiguredText(string(target), 64) || target == p.Name {
			return errors.New("specialist handoff targets must be distinct non-empty specialist names")
		}
	}
	return nil
}

type Registry struct {
	profiles map[domain.SpecialistName]Profile
}

func NewRegistry(profiles ...Profile) (*Registry, error) {
	registry := &Registry{profiles: make(map[domain.SpecialistName]Profile, len(profiles))}
	for _, profile := range profiles {
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("invalid specialist %q: %w", profile.Name, err)
		}
		if _, exists := registry.profiles[profile.Name]; exists {
			return nil, fmt.Errorf("duplicate specialist %q", profile.Name)
		}
		profile.AllowedCollectors = append([]domain.CapabilityName(nil), profile.AllowedCollectors...)
		profile.AllowedCapabilities = append([]domain.CapabilityName(nil), profile.AllowedCapabilities...)
		profile.HandoffTargets = append([]domain.SpecialistName(nil), profile.HandoffTargets...)
		registry.profiles[profile.Name] = profile
	}
	for _, profile := range registry.profiles {
		for _, target := range profile.HandoffTargets {
			if _, exists := registry.profiles[target]; !exists {
				return nil, fmt.Errorf("specialist %q has unknown handoff target %q", profile.Name, target)
			}
		}
	}
	return registry, nil
}

func (r *Registry) Get(name domain.SpecialistName) (Profile, bool) {
	profile, ok := r.profiles[name]
	if !ok {
		return Profile{}, false
	}
	profile.AllowedCollectors = append([]domain.CapabilityName(nil), profile.AllowedCollectors...)
	profile.AllowedCapabilities = append([]domain.CapabilityName(nil), profile.AllowedCapabilities...)
	profile.HandoffTargets = append([]domain.SpecialistName(nil), profile.HandoffTargets...)
	return profile, true
}

func (r *Registry) Profiles() []Profile {
	profiles := make([]Profile, 0, len(r.profiles))
	for _, profile := range r.profiles {
		profile.AllowedCollectors = append([]domain.CapabilityName(nil), profile.AllowedCollectors...)
		profile.AllowedCapabilities = append([]domain.CapabilityName(nil), profile.AllowedCapabilities...)
		profile.HandoffTargets = append([]domain.SpecialistName(nil), profile.HandoffTargets...)
		profiles = append(profiles, profile)
	}
	return profiles
}

func (r *Registry) AllowsCollector(name domain.SpecialistName, capability domain.CapabilityName) bool {
	profile, ok := r.profiles[name]
	return ok && containsCapability(profile.AllowedCollectors, capability)
}

func (r *Registry) AllowsCapability(name domain.SpecialistName, capability domain.CapabilityName) bool {
	profile, ok := r.profiles[name]
	return ok && containsCapability(profile.AllowedCapabilities, capability)
}

func (r *Registry) AllowsHandoff(from, to domain.SpecialistName) bool {
	profile, ok := r.profiles[from]
	return ok && containsSpecialist(profile.HandoffTargets, to)
}

func containsCapability(values []domain.CapabilityName, want domain.CapabilityName) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSpecialist(values []domain.SpecialistName, want domain.SpecialistName) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func NewDispatcher(registry *Registry) (*Dispatcher, error) {
	return NewDispatcherWithRules(registry, nil)
}

// RouteMatch is evaluated only against structured connector fields. Values
// within a selector are alternatives; selectors themselves are all required.
// This lets an administrator make Jira request type authoritative while still
// requiring a component or custom field for a more specific route.
type RouteMatch struct {
	Sources      []string            `yaml:"sources"`
	IssueTypes   []string            `yaml:"issue_types"`
	RequestTypes []string            `yaml:"request_types"`
	Components   []string            `yaml:"components"`
	Labels       []string            `yaml:"labels"`
	Fields       map[string][]string `yaml:"fields"`
}

// Reject explicitly empty or misspelled selectors rather than silently
// broadening an administrator's rule. A custom decoder must preserve the
// enclosing configuration decoder's strict unknown-field behavior.
func (m *RouteMatch) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("route match must be a selector mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "sources", "issue_types", "request_types", "components", "labels":
			if value.Kind != yaml.SequenceNode || len(value.Content) == 0 {
				return fmt.Errorf("route selector %q must contain at least one value", key)
			}
		case "fields":
			if value.Kind != yaml.MappingNode || len(value.Content) == 0 {
				return errors.New("route fields selector must contain at least one field")
			}
		default:
			return fmt.Errorf("unknown route selector %q", key)
		}
	}
	type plain RouteMatch
	return node.Decode((*plain)(m))
}

type RouteRule struct {
	Name           string                `yaml:"name"`
	Specialist     domain.SpecialistName `yaml:"specialist"`
	Classification string                `yaml:"classification"`
	Match          RouteMatch            `yaml:"match"`
}

func (r RouteRule) Validate(registry *Registry) error {
	if !validConfiguredText(r.Name, 128) {
		return errors.New("route name is required and must be at most 128 bytes")
	}
	if r.Specialist == "" {
		return errors.New("route specialist is required")
	}
	if registry == nil {
		return errors.New("specialist registry is required")
	}
	if _, ok := registry.profiles[r.Specialist]; !ok {
		return fmt.Errorf("route %q references unknown specialist %q", r.Name, r.Specialist)
	}
	if !validConfiguredText(r.Classification, 128) {
		return errors.New("route classification is required and must be at most 128 bytes")
	}
	if !r.Match.hasSelector() {
		return errors.New("route match requires at least one selector")
	}
	for _, values := range [][]string{r.Match.Sources, r.Match.IssueTypes, r.Match.RequestTypes, r.Match.Components, r.Match.Labels} {
		if values != nil && len(values) == 0 {
			return errors.New("route selectors must contain at least one value")
		}
		if err := validateRouteValues(values); err != nil {
			return err
		}
	}
	if len(r.Match.Fields) > 64 {
		return errors.New("route exceeds 64 custom field selectors")
	}
	seenFields := make(map[string]struct{}, len(r.Match.Fields))
	for field, values := range r.Match.Fields {
		if !validConfiguredText(field, 128) || len(values) == 0 {
			return errors.New("route field selectors require a name and at least one value")
		}
		key := strings.TrimSpace(field)
		if _, duplicate := seenFields[key]; duplicate {
			return fmt.Errorf("duplicate trimmed route field %q", key)
		}
		seenFields[key] = struct{}{}
		if err := validateRouteValues(values); err != nil {
			return err
		}
	}
	return nil
}

func validateRouteValues(values []string) error {
	if len(values) > 32 {
		return errors.New("route selectors must have at most 32 alternatives")
	}
	for _, value := range values {
		if !validConfiguredText(value, 512) {
			return errors.New("route selector values must be nonblank valid UTF-8 without NUL and at most 512 bytes")
		}
	}
	return nil
}

func validConfiguredText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) && strings.IndexByte(value, 0) < 0 && strings.TrimSpace(value) != ""
}

func (m RouteMatch) hasSelector() bool {
	return len(m.Sources) > 0 || len(m.IssueTypes) > 0 || len(m.RequestTypes) > 0 || len(m.Components) > 0 || len(m.Labels) > 0 || len(m.Fields) > 0
}

type Dispatcher struct {
	registry *Registry
	rules    []compiledRule
}

const (
	MaxRoutingRules     = 128
	maxRoutingSelectors = 4096
	maxRoutingValues    = 16384
)

// IntentConfig limits the optional intent model call after no rule matches.
type IntentConfig struct {
	Enabled       bool          `yaml:"enabled"`
	MinConfidence float64       `yaml:"min_confidence"`
	Timeout       time.Duration `yaml:"timeout"`
}

func DefaultIntentConfig() IntentConfig {
	return IntentConfig{Enabled: true, MinConfidence: 0.75, Timeout: 10 * time.Second}
}

func (c IntentConfig) Validate() error {
	if math.IsNaN(c.MinConfidence) || math.IsInf(c.MinConfidence, 0) || c.MinConfidence <= 0 || c.MinConfidence > 1 {
		return errors.New("intent minimum confidence must be finite and greater than zero and at most one")
	}
	if c.Timeout <= 0 || c.Timeout > 5*time.Minute {
		return errors.New("intent timeout must be positive and at most five minutes")
	}
	return nil
}

type compiledField struct {
	key    string
	values []string
}

type compiledRule struct {
	decision                                              domain.RoutingDecision
	sources, issueTypes, requestTypes, components, labels []string
	fields                                                []compiledField
}

func NewDispatcherWithRules(registry *Registry, rules []RouteRule) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("specialist registry is required")
	}
	if _, ok := registry.profiles[HelpDesk]; !ok {
		return nil, errors.New("help desk specialist is required as the safe fallback")
	}
	if len(rules) > MaxRoutingRules {
		return nil, fmt.Errorf("routing exceeds %d rules", MaxRoutingRules)
	}
	compiled := make([]compiledRule, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	selectors, values := 0, 0
	for _, rule := range rules {
		if err := rule.Validate(registry); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(rule.Name)
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate route %q", rule.Name)
		}
		seen[name] = struct{}{}
		for _, alternatives := range [][]string{rule.Match.Sources, rule.Match.IssueTypes, rule.Match.RequestTypes, rule.Match.Components, rule.Match.Labels} {
			if len(alternatives) > 0 {
				selectors++
			}
			values += len(alternatives)
		}
		selectors += len(rule.Match.Fields)
		for _, alternatives := range rule.Match.Fields {
			values += len(alternatives)
		}
		if selectors > maxRoutingSelectors || values > maxRoutingValues {
			return nil, errors.New("routing exceeds total selector or alternative limits")
		}
		compiledRule := compileRule(rule)
		if err := compiledRule.decision.Validate(); err != nil {
			return nil, fmt.Errorf("route %q: %w", name, err)
		}
		compiled = append(compiled, compiledRule)
	}
	return &Dispatcher{registry: registry, rules: compiled}, nil
}

func compileRule(rule RouteRule) compiledRule {
	name := strings.TrimSpace(rule.Name)
	compiled := compiledRule{
		decision: domain.RoutingDecision{Specialist: rule.Specialist, Classification: strings.TrimSpace(rule.Classification), Source: domain.RoutingSourceRule, RuleName: name, Reason: "matched structured routing rule " + name},
		sources:  compileValues(rule.Match.Sources), issueTypes: compileValues(rule.Match.IssueTypes),
		requestTypes: compileValues(rule.Match.RequestTypes), components: compileValues(rule.Match.Components), labels: compileValues(rule.Match.Labels),
		fields: make([]compiledField, 0, len(rule.Match.Fields)),
	}
	for field, values := range rule.Match.Fields {
		compiled.fields = append(compiled.fields, compiledField{key: strings.TrimSpace(field), values: compileValues(values)})
	}
	// Stable field order makes early exits and dispatch cost predictable.
	sort.Slice(compiled.fields, func(i, j int) bool { return compiled.fields[i].key < compiled.fields[j].key })
	return compiled
}

func compileValues(values []string) []string {
	result := make([]string, len(values))
	for i, value := range values {
		// Keep the original Unicode case for EqualFold. Lowercasing only one
		// side changes semantics for characters such as dotted capital I.
		result[i] = strings.TrimSpace(value)
	}
	return result
}

type Decision = domain.RoutingDecision

func (d *Dispatcher) Dispatch(ticket domain.Ticket) domain.RoutingDecision {
	for _, rule := range d.rules {
		if rule.matches(ticket) {
			return rule.decision
		}
	}
	return domain.RoutingDecision{Specialist: HelpDesk, Classification: "help_desk", Source: domain.RoutingSourceFallback, FallbackCode: "no_rule_match", Reason: "no structured routing rule matched; defaulting to help desk"}
}

func (r compiledRule) matches(ticket domain.Ticket) bool {
	m := ticket.Metadata
	if !matchesAny(string(ticket.Source), r.sources) || !matchesAny(m.IssueType, r.issueTypes) ||
		!matchesAny(m.RequestType, r.requestTypes) || !matchesList(m.Components, r.components) || !matchesList(m.Labels, r.labels) {
		return false
	}
	for _, field := range r.fields {
		actual, ok := m.Fields[field.key]
		if !ok {
			for key, alternatives := range m.Fields {
				if strings.TrimSpace(key) == field.key {
					actual, ok = alternatives, true
					break
				}
			}
		}
		if !ok || !matchesList(actual, field.values) {
			return false
		}
	}
	return true
}

func matchesAny(actual string, expected []string) bool {
	if len(expected) == 0 {
		return true
	}
	actual = strings.TrimSpace(actual)
	for _, value := range expected {
		if strings.EqualFold(value, actual) {
			return true
		}
	}
	return false
}

func matchesList(actual, expected []string) bool {
	if len(expected) == 0 {
		return true
	}
	for _, value := range actual {
		if matchesAny(value, expected) {
			return true
		}
	}
	return false
}

// Candidates returns only configured specialist names, in a stable order, for
// a model intent classifier. It never exposes or grants permissions.
func (d *Dispatcher) Candidates() []domain.SpecialistName {
	candidates := make([]domain.SpecialistName, 0, len(d.registry.profiles))
	for name := range d.registry.profiles {
		candidates = append(candidates, name)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
	return candidates
}
