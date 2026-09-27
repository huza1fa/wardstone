package investigations

import (
	"context"
	"errors"
	"time"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
)

var (
	ErrNotFound          = errors.New("investigation not found")
	ErrInvalidTransition = errors.New("invalid investigation transition")
)

type CollectionFailure struct {
	Source   domain.ConnectorName
	Error    string
	Duration time.Duration
}

type ActionEvaluation struct {
	Action domain.ProposedAction
	Policy domain.PolicyResult
}

type ToolInvocation struct {
	Connector  domain.ConnectorName
	Capability domain.CapabilityName
}

type Store interface {
	ReceiveTicket(context.Context, domain.Ticket, domain.Investigation) (domain.InvestigationID, bool, error)
	GetInvestigation(context.Context, domain.InvestigationID) (domain.Investigation, error)
	GetTicket(context.Context, domain.InvestigationID) (domain.Ticket, error)
	StartInvestigation(context.Context, domain.InvestigationID, time.Time) error
	RecordToolInvocations(context.Context, domain.InvestigationID, []ToolInvocation, time.Time) error
	RecordEvidence(context.Context, domain.InvestigationID, []domain.Evidence, []CollectionFailure, time.Time) error
	CompleteInvestigation(context.Context, domain.InvestigationID, string, domain.ModelProviderName, string, []ActionEvaluation, time.Time) error
	FailInvestigation(context.Context, domain.InvestigationID, string, time.Time) error
	Timeline(context.Context, domain.InvestigationID) ([]audit.Event, error)
}
