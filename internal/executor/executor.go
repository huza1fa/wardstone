package executor

import (
	"context"
	"encoding/json"

	"github.com/wardstone-project/wardstone/internal/domain"
)

// Mutator is implemented only by privileged connector adapters. It must not be
// exposed to the model or sandbox layers.
type Mutator interface {
	Execute(context.Context, domain.CapabilityName, json.RawMessage, string) (json.RawMessage, error)
}

type Verifier interface {
	Verify(context.Context, domain.ProposedAction, domain.Execution) (domain.Verification, error)
}
