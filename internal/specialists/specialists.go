// Package specialists defines the least-privilege investigation roles used by
// the shared Wardstone runtime. Profiles are configuration, not agents: they
// cannot create capabilities or bypass policy.
package specialists

import (
	"errors"
	"fmt"
	"sort"
	"strings"

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
	if p.Name == "" || len(p.Name) > 64 {
		return errors.New("specialist name is required and must be at most 64 bytes")
	}
	if strings.TrimSpace(p.Instructions) == "" || len(p.Instructions) > 8000 {
		return errors.New("specialist instructions are required and must be at most 8000 bytes")
	}
	if len(p.AllowedCollectors) > 128 || len(p.AllowedCapabilities) > 128 || len(p.HandoffTargets) > 32 {
		return errors.New("specialist allowlists exceed configured safety limits")
	}
	for _, capability := range append(append([]domain.CapabilityName(nil), p.AllowedCollectors...), p.AllowedCapabilities...) {
		if capability == "" || len(capability) > 128 {
			return errors.New("specialist capability names must be non-empty and at most 128 bytes")
		}
	}
	for _, target := range p.HandoffTargets {
		if target == "" || len(target) > 64 || target == p.Name {
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
	IssueTypes   []string            `yaml:"issue_types"`
	RequestTypes []string            `yaml:"request_types"`
	Components   []string            `yaml:"components"`
	Labels       []string            `yaml:"labels"`
	Fields       map[string][]string `yaml:"fields"`
}

type RouteRule struct {
	Name           string                `yaml:"name"`
	Specialist     domain.SpecialistName `yaml:"specialist"`
	Classification string                `yaml:"classification"`
	Match          RouteMatch            `yaml:"match"`
}

func (r RouteRule) Validate(registry *Registry) error {
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 128 {
		return errors.New("route name is required and must be at most 128 bytes")
	}
	if r.Specialist == "" {
		return errors.New("route specialist is required")
	}
	if _, ok := registry.Get(r.Specialist); !ok {
		return fmt.Errorf("route %q references unknown specialist %q", r.Name, r.Specialist)
	}
	if strings.TrimSpace(r.Classification) == "" || len(r.Classification) > 128 {
		return errors.New("route classification is required and must be at most 128 bytes")
	}
	if !r.Match.hasSelector() {
		return errors.New("route match requires at least one selector")
	}
	for _, values := range [][]string{r.Match.IssueTypes, r.Match.RequestTypes, r.Match.Components, r.Match.Labels} {
		if err := validateRouteValues(values); err != nil {
			return err
		}
	}
	for field, values := range r.Match.Fields {
		if strings.TrimSpace(field) == "" || len(field) > 128 || len(values) == 0 {
			return errors.New("route field selectors require a name and at least one value")
		}
		if err := validateRouteValues(values); err != nil {
			return err
		}
	}
	return nil
}

func validateRouteValues(values []string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 512 {
			return errors.New("route selector values must be non-empty and at most 512 bytes")
		}
	}
	return nil
}

func (m RouteMatch) hasSelector() bool {
	return len(m.IssueTypes) > 0 || len(m.RequestTypes) > 0 || len(m.Components) > 0 || len(m.Labels) > 0 || len(m.Fields) > 0
}

type Dispatcher struct {
	registry *Registry
	rules    []RouteRule
}

func NewDispatcherWithRules(registry *Registry, rules []RouteRule) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("specialist registry is required")
	}
	if _, ok := registry.Get(HelpDesk); !ok {
		return nil, errors.New("help desk specialist is required as the safe fallback")
	}
	copyRules := make([]RouteRule, len(rules))
	for i, rule := range rules {
		copyRules[i] = cloneRouteRule(rule)
	}
	seen := make(map[string]struct{}, len(copyRules))
	for _, rule := range copyRules {
		if err := rule.Validate(registry); err != nil {
			return nil, err
		}
		if _, duplicate := seen[rule.Name]; duplicate {
			return nil, fmt.Errorf("duplicate route %q", rule.Name)
		}
		seen[rule.Name] = struct{}{}
	}
	return &Dispatcher{registry: registry, rules: copyRules}, nil
}

func cloneRouteRule(rule RouteRule) RouteRule {
	rule.Match.IssueTypes = append([]string(nil), rule.Match.IssueTypes...)
	rule.Match.RequestTypes = append([]string(nil), rule.Match.RequestTypes...)
	rule.Match.Components = append([]string(nil), rule.Match.Components...)
	rule.Match.Labels = append([]string(nil), rule.Match.Labels...)
	if len(rule.Match.Fields) != 0 {
		fields := make(map[string][]string, len(rule.Match.Fields))
		for field, values := range rule.Match.Fields {
			fields[field] = append([]string(nil), values...)
		}
		rule.Match.Fields = fields
	}
	return rule
}

type Decision struct {
	Specialist     domain.SpecialistName
	Classification string
	Reason         string
	NeedsIntent    bool
}

func (d *Dispatcher) Dispatch(ticket domain.Ticket) Decision {
	for _, rule := range d.rules {
		if rule.Match.matches(ticket.Metadata) {
			return Decision{Specialist: rule.Specialist, Classification: rule.Classification, Reason: "matched Jira routing rule " + rule.Name}
		}
	}
	// Ticket content is untrusted. Bound the data considered by the lightweight
	// classifier so hostile or unusually large payloads cannot consume work. It
	// preserves the original safe access-management route, but exact connector
	// fields above take precedence.
	text := strings.ToLower(bounded(ticket.Summary, 8*1024) + "\n" + bounded(ticket.Description, 8*1024))
	for _, term := range []string{"access", "permission", "entitlement", "group membership", "license", "licence", "role assignment"} {
		if strings.Contains(text, term) {
			if _, ok := d.registry.Get(AccessManagement); ok {
				return Decision{Specialist: AccessManagement, Classification: "access_management", Reason: "ticket matches access-management routing terms"}
			}
		}
	}
	return Decision{Specialist: HelpDesk, Classification: "help_desk", Reason: "no deterministic route matched; defaulting safely to help desk", NeedsIntent: true}
}

func (m RouteMatch) matches(metadata domain.TicketMetadata) bool {
	return matchesAny(metadata.IssueType, m.IssueTypes) &&
		matchesAny(metadata.RequestType, m.RequestTypes) &&
		matchesList(metadata.Components, m.Components) &&
		matchesList(metadata.Labels, m.Labels) &&
		matchesFields(metadata.Fields, m.Fields)
}

func matchesAny(actual string, expected []string) bool {
	if len(expected) == 0 {
		return true
	}
	for _, value := range expected {
		if normalize(value) == normalize(actual) {
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

func matchesFields(actual, expected map[string][]string) bool {
	for field, values := range expected {
		matched := false
		for actualField, actualValues := range actual {
			if normalize(actualField) == normalize(field) && matchesList(actualValues, values) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

// Candidates returns only configured specialists, in a stable order, for a
// model intent classifier. It exposes role descriptions but never permissions.
func (d *Dispatcher) Candidates() []domain.SpecialistName {
	profiles := d.registry.Profiles()
	candidates := make([]domain.SpecialistName, 0, len(profiles))
	for _, profile := range profiles {
		candidates = append(candidates, profile.Name)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
	return candidates
}

func bounded(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
