package connectors

import (
	"context"

	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/domain"
)

type Registrar interface {
	Name() domain.ConnectorName
	Capabilities() []capabilities.Definition
}

type EvidenceCollector interface {
	Name() domain.ConnectorName
	Capability() domain.CapabilityName
	Collect(context.Context, domain.Ticket) ([]domain.Evidence, error)
}
