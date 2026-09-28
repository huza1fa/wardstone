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
	Conversation    []domain.CaseMessage
	Warnings        []string
	Specialist      domain.SpecialistName `json:"specialist"`
	Instructions    string                `json:"instructions"`
}

type ProposedAction struct {
	Capability  domain.CapabilityName `json:"capability"`
	Arguments   json.RawMessage       `json:"arguments"`
	Reason      string                `json:"reason"`
	EvidenceIDs []domain.EvidenceID   `json:"evidence_ids"`
}

type Result struct {
	Diagnosis        string           `json:"diagnosis"`
	FollowUpQuestion string           `json:"follow_up_question"`
	Actions          []ProposedAction `json:"actions"`
	Handoff          *Handoff         `json:"handoff,omitempty"`
}

// Handoff asks the deterministic runtime to resume the same durable case with
// another installed specialist. It does not grant that specialist authority.
type Handoff struct {
	Specialist domain.SpecialistName `json:"specialist"`
	Reason     string                `json:"reason"`
}

type ModelProvider interface {
	Name() domain.ModelProviderName
	Model() string
	Diagnose(context.Context, Request) (Result, error)
}
