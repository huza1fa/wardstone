package capabilities

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var (
	ErrNotFound  = errors.New("capability not found")
	ErrDuplicate = errors.New("capability already registered")
)

type Definition struct {
	Name              domain.CapabilityName
	Connector         domain.ConnectorName
	Description       string
	Effect            domain.CapabilityEffect
	ArgumentsVersion  int
	Idempotent        bool
	ValidateArguments func(json.RawMessage) error
}

func (d Definition) Validate() error {
	if d.Name == "" || d.Connector == "" || d.Description == "" {
		return errors.New("name, connector, and description are required")
	}
	if d.Effect != domain.EffectRead && d.Effect != domain.EffectMutate {
		return errors.New("effect must be READ or MUTATE")
	}
	if d.ArgumentsVersion < 1 {
		return errors.New("arguments version must be positive")
	}
	return nil
}

type Registry struct {
	mu          sync.RWMutex
	definitions map[domain.CapabilityName]Definition
}

func NewRegistry() *Registry {
	return &Registry{definitions: make(map[domain.CapabilityName]Definition)}
}

func (r *Registry) Register(definitions ...Definition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return fmt.Errorf("capability %q: %w", definition.Name, err)
		}
		if _, exists := r.definitions[definition.Name]; exists {
			return fmt.Errorf("%w: %s", ErrDuplicate, definition.Name)
		}
		r.definitions[definition.Name] = definition
	}
	return nil
}

func (r *Registry) Get(name domain.CapabilityName) (Definition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	definition, ok := r.definitions[name]
	if !ok {
		return Definition{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return definition, nil
}

func (r *Registry) ValidateAction(action domain.ProposedAction) error {
	definition, err := r.Get(action.Capability)
	if err != nil {
		return err
	}
	if !action.DigestValid() {
		return errors.New("action digest is invalid")
	}
	if definition.ValidateArguments != nil {
		if err := definition.ValidateArguments(action.Arguments); err != nil {
			return fmt.Errorf("invalid arguments for %s: %w", action.Capability, err)
		}
	}
	return nil
}
