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
