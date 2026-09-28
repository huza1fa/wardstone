// Package specialists defines the least-privilege investigation roles used by
// the shared Wardstone runtime. Profiles are configuration, not agents: they
// cannot create capabilities or bypass policy.
package specialists

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const (
	HelpDesk         domain.SpecialistName = "help_desk"
	AccessManagement domain.SpecialistName = "access_management"
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

// Dispatcher is deliberately deterministic. It has no connector or mutation
// capability and routes only among profiles already installed in its registry.
type Dispatcher struct{ registry *Registry }

func NewDispatcher(registry *Registry) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("specialist registry is required")
	}
	if _, ok := registry.Get(HelpDesk); !ok {
		return nil, errors.New("help desk specialist is required as the safe fallback")
	}
	return &Dispatcher{registry: registry}, nil
}

type Decision struct {
	Specialist     domain.SpecialistName
	Classification string
	Reason         string
}

func (d *Dispatcher) Dispatch(ticket domain.Ticket) Decision {
	// Ticket content is untrusted. Bound the data considered by the lightweight
	// classifier so hostile or unusually large payloads cannot consume work.
	text := strings.ToLower(bounded(ticket.Summary, 8*1024) + "\n" + bounded(ticket.Description, 8*1024))
	for _, term := range []string{"access", "permission", "entitlement", "group membership", "license", "licence", "role assignment"} {
		if strings.Contains(text, term) {
			if _, ok := d.registry.Get(AccessManagement); ok {
				return Decision{Specialist: AccessManagement, Classification: "access_management", Reason: "ticket matches access-management routing terms"}
			}
		}
	}
	return Decision{Specialist: HelpDesk, Classification: "help_desk", Reason: "default route for user-facing or unclassified operational work"}
}

func bounded(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
