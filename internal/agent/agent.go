package agent

import (
	"context"
	"encoding/json"

	"github.com/wardstone-project/wardstone/internal/domain"
)

type Request struct {
	InvestigationID domain.InvestigationID
	Ticket          domain.Ticket
	Evidence        []domain.Evidence
	Warnings        []string
}

type ProposedAction struct {
	Capability  domain.CapabilityName `json:"capability"`
	Arguments   json.RawMessage       `json:"arguments"`
	Reason      string                `json:"reason"`
	EvidenceIDs []domain.EvidenceID   `json:"evidence_ids"`
}

type Result struct {
	Diagnosis string           `json:"diagnosis"`
	Actions   []ProposedAction `json:"actions"`
}

type ModelProvider interface {
	Name() domain.ModelProviderName
	Model() string
	Diagnose(context.Context, Request) (Result, error)
}
