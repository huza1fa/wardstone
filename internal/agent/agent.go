package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const MaxDiagnosisPromptBytes = 256 << 10

type Request struct {
	InvestigationID domain.InvestigationID
	Ticket          domain.Ticket
	Evidence        []domain.Evidence
	Conversation    []domain.CaseMessage
	Warnings        []string
	Handoffs        []HandoffContext
	Specialist      domain.SpecialistName `json:"specialist"`
	Instructions    string                `json:"instructions"`
}

// HandoffContext is durable history, not a source of instructions or authority.
type HandoffContext struct {
	From       domain.SpecialistName `json:"from"`
	To         domain.SpecialistName `json:"to"`
	Reason     string                `json:"reason"`
	OccurredAt time.Time             `json:"occurred_at"`
}

type ProposedAction struct {
	Capability  domain.CapabilityName `json:"capability"`
	Arguments   json.RawMessage       `json:"arguments"`
	Reason      string                `json:"reason"`
	EvidenceIDs []domain.EvidenceID   `json:"evidence_ids"`
}

type Result struct {
	Diagnosis        string              `json:"diagnosis"`
	FollowUpQuestion string              `json:"follow_up_question"`
	Actions          []ProposedAction    `json:"actions"`
	Handoff          *Handoff            `json:"handoff,omitempty"`
	WorkPackage      *domain.WorkPackage `json:"work_package,omitempty"`
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

// IntentClassifier is deliberately narrower than ModelProvider. The runtime
// uses it only after deterministic connector-field routing has no match. Its
// result selects from installed candidates; it cannot attach tools, actions,
// or authority to a ticket.
type IntentClassifier interface {
	ClassifyIntent(context.Context, IntentRequest) (IntentResult, error)
}

type IntentRequest struct {
	Ticket     domain.Ticket     `json:"ticket"`
	Candidates []IntentCandidate `json:"candidates"`
}

type IntentCandidate struct {
	Specialist     domain.SpecialistName `json:"specialist"`
	Classification string                `json:"classification"`
	Description    string                `json:"description"`
}

type IntentResult struct {
	Specialist     domain.SpecialistName `json:"specialist"`
	Classification string                `json:"classification"`
	Confidence     float64               `json:"confidence"`
	Reason         string                `json:"reason"`
}
