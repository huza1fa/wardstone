package audit

import (
	"encoding/json"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

type EventType string

const (
	TicketReceived            EventType = "ticket.received"
	DispatcherRouted          EventType = "dispatcher.routed"
	SpecialistStarted         EventType = "specialist.started"
	SpecialistHandedOff       EventType = "specialist.handed_off"
	SpecialistCompleted       EventType = "specialist.completed"
	InvestigationStarted      EventType = "investigation.started"
	ToolInvoked               EventType = "tool.invoked"
	EvidenceCollected         EventType = "evidence.collected"
	EvidenceCollectionFailed  EventType = "evidence.collection_failed"
	DiagnosisGenerated        EventType = "diagnosis.generated"
	ActionProposed            EventType = "action.proposed"
	PolicyEvaluated           EventType = "policy.evaluated"
	ApprovalRequested         EventType = "approval.requested"
	ApprovalGranted           EventType = "approval.granted"
	ApprovalDenied            EventType = "approval.denied"
	ApprovalExpired           EventType = "approval.expired"
	ActionExecuted            EventType = "action.executed"
	ActionFailed              EventType = "action.failed"
	VerificationCompleted     EventType = "verification.completed"
	TicketUpdated             EventType = "ticket.updated"
	InvestigationCompleted    EventType = "investigation.completed"
	InvestigationFailed       EventType = "investigation.failed"
	RequesterQuestionAsked    EventType = "requester.question_asked"
	RequesterReplyReceived    EventType = "requester.reply_received"
	InvestigationResumed      EventType = "investigation.resumed"
	RequesterMessageDelivered EventType = "requester.message_delivered"
	RequesterMessageFailed    EventType = "requester.message_failed"
	RequesterMessageReviewed  EventType = "requester.message_reviewed"
)

type ActorType string

const (
	ActorSystem    ActorType = "SYSTEM"
	ActorModel     ActorType = "MODEL"
	ActorOperator  ActorType = "OPERATOR"
	ActorConnector ActorType = "CONNECTOR"
)

type Event struct {
	ID              int64                  `json:"id"`
	InvestigationID domain.InvestigationID `json:"investigation_id"`
	Sequence        int64                  `json:"sequence"`
	Type            EventType              `json:"type"`
	ActorType       ActorType              `json:"actor_type"`
	ActorID         string                 `json:"actor_id,omitempty"`
	OccurredAt      time.Time              `json:"occurred_at"`
	Data            json.RawMessage        `json:"data"`
}

func Data(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

// EvidenceData retains source provenance without copying document contents or
// local filesystem paths into the audit timeline.
func EvidenceData(item domain.Evidence) map[string]any {
	result := map[string]any{"evidence_id": item.ID, "capability": item.Capability, "kind": item.Kind, "observed_at": item.ObservedAt}
	if item.Source == "research" {
		var document struct {
			Title     string `json:"title"`
			URL       string `json:"url"`
			Digest    string `json:"content_sha256"`
			ClaimType string `json:"claim_type"`
			Truncated bool   `json:"truncated"`
		}
		if json.Unmarshal(item.Data, &document) == nil {
			result["title"], result["source_url"], result["content_sha256"], result["claim_type"], result["truncated"] = document.Title, document.URL, document.Digest, document.ClaimType, document.Truncated
		}
	}
	return result
}
