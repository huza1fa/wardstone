package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type OperatingMode string

const (
	OperatingModeShadow     OperatingMode = "SHADOW"
	OperatingModeApproval   OperatingMode = "APPROVAL"
	OperatingModeAutonomous OperatingMode = "AUTONOMOUS"
)

func (m OperatingMode) Valid() bool {
	return m == OperatingModeShadow || m == OperatingModeApproval || m == OperatingModeAutonomous
}

type CapabilityEffect string

const (
	EffectRead   CapabilityEffect = "READ"
	EffectMutate CapabilityEffect = "MUTATE"
)

type InvestigationStatus string

const (
	InvestigationPending   InvestigationStatus = "PENDING"
	InvestigationRunning   InvestigationStatus = "RUNNING"
	InvestigationWaiting   InvestigationStatus = "WAITING_ON_REQUESTER"
	InvestigationCompleted InvestigationStatus = "COMPLETED"
	InvestigationFailed    InvestigationStatus = "FAILED"
	InvestigationCancelled InvestigationStatus = "CANCELLED"
)

type MessageDirection string

const (
	MessageOutbound MessageDirection = "OUTBOUND"
	MessageInbound  MessageDirection = "INBOUND"
)

type DeliveryStatus string

const (
	DeliveryPending DeliveryStatus = "PENDING"
	DeliveryRunning DeliveryStatus = "RUNNING"
	DeliverySent    DeliveryStatus = "SENT"
	DeliveryDead    DeliveryStatus = "DEAD"
)

type PolicyDecision string

const (
	PolicyAllow           PolicyDecision = "ALLOW"
	PolicyRequireApproval PolicyDecision = "REQUIRE_APPROVAL"
	PolicyDeny            PolicyDecision = "DENY"
)

type ApprovalStatus string

const (
	ApprovalPending ApprovalStatus = "PENDING"
	ApprovalGranted ApprovalStatus = "GRANTED"
	ApprovalDenied  ApprovalStatus = "DENIED"
	ApprovalExpired ApprovalStatus = "EXPIRED"
)

type ExecutionStatus string

const (
	ExecutionPending   ExecutionStatus = "PENDING"
	ExecutionRunning   ExecutionStatus = "RUNNING"
	ExecutionSucceeded ExecutionStatus = "SUCCEEDED"
	ExecutionFailed    ExecutionStatus = "FAILED"
)

type Ticket struct {
	ID            TicketID      `json:"id"`
	Source        ConnectorName `json:"source"`
	ExternalID    string        `json:"external_id"`
	Summary       string        `json:"summary"`
	Description   string        `json:"description"`
	ReporterEmail string        `json:"reporter_email,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
}

func (t Ticket) Validate() error {
	if t.ID == "" || t.Source == "" || t.ExternalID == "" || t.Summary == "" {
		return errors.New("ticket ID, source, external ID, and summary are required")
	}
	return nil
}

type Investigation struct {
	ID            InvestigationID     `json:"id"`
	TicketID      TicketID            `json:"ticket_id"`
	Status        InvestigationStatus `json:"status"`
	ModelProvider ModelProviderName   `json:"model_provider,omitempty"`
	Model         string              `json:"model,omitempty"`
	PromptVersion string              `json:"prompt_version"`
	Diagnosis     string              `json:"diagnosis,omitempty"`
	Failure       string              `json:"failure,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	StartedAt     *time.Time          `json:"started_at,omitempty"`
	CompletedAt   *time.Time          `json:"completed_at,omitempty"`
}

// CaseMessage is durable conversation context. ExternalID is the connector's
// stable message/comment ID and is used to make webhook delivery idempotent.
type CaseMessage struct {
	ID              MessageID        `json:"id"`
	InvestigationID InvestigationID  `json:"investigation_id"`
	Source          ConnectorName    `json:"source"`
	ExternalID      string           `json:"external_id"`
	Direction       MessageDirection `json:"direction"`
	Author          string           `json:"author,omitempty"`
	Body            string           `json:"body"`
	CreatedAt       time.Time        `json:"created_at"`
}

type MessageDelivery struct {
	ID             DeliveryID     `json:"id"`
	MessageID      MessageID      `json:"message_id"`
	Status         DeliveryStatus `json:"status"`
	Attempt        int            `json:"attempt"`
	RemoteID       string         `json:"remote_id,omitempty"`
	LastError      string         `json:"last_error,omitempty"`
	AvailableAt    time.Time      `json:"available_at"`
	LeaseOwner     string         `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time     `json:"lease_expires_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	DeliveredAt    *time.Time     `json:"delivered_at,omitempty"`
}

type Evidence struct {
	ID              EvidenceID      `json:"id"`
	InvestigationID InvestigationID `json:"investigation_id"`
	Source          ConnectorName   `json:"source"`
	Kind            string          `json:"kind"`
	Summary         string          `json:"summary"`
	Data            json.RawMessage `json:"data"`
	ObservedAt      time.Time       `json:"observed_at"`
}

type ProposedAction struct {
	ID              ActionID        `json:"id"`
	InvestigationID InvestigationID `json:"investigation_id"`
	Capability      CapabilityName  `json:"capability"`
	Arguments       json.RawMessage `json:"arguments"`
	Reason          string          `json:"reason"`
	EvidenceIDs     []EvidenceID    `json:"evidence_ids"`
	Digest          string          `json:"digest"`
	CreatedAt       time.Time       `json:"created_at"`
}

func (a *ProposedAction) Seal() error {
	if a.ID == "" || a.InvestigationID == "" || a.Capability == "" {
		return errors.New("action ID, investigation ID, and capability are required")
	}
	canonical, err := canonicalJSON(a.Arguments)
	if err != nil {
		return fmt.Errorf("canonicalize action arguments: %w", err)
	}
	a.Arguments = canonical
	payload := struct {
		Version    int             `json:"version"`
		Capability CapabilityName  `json:"capability"`
		Arguments  json.RawMessage `json:"arguments"`
	}{Version: 1, Capability: a.Capability, Arguments: canonical}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode action digest payload: %w", err)
	}
	digest := sha256.Sum256(encoded)
	a.Digest = hex.EncodeToString(digest[:])
	return nil
}

func (a ProposedAction) DigestValid() bool {
	copy := a
	if copy.Seal() != nil {
		return false
	}
	return copy.Digest == a.Digest
}

func canonicalJSON(input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		return nil, errors.New("JSON value is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("multiple JSON values")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

type PolicyResult struct {
	Decision PolicyDecision `json:"decision"`
	Reason   string         `json:"reason"`
}

type Approval struct {
	ID           ApprovalID     `json:"id"`
	ActionID     ActionID       `json:"action_id"`
	ActionDigest string         `json:"action_digest"`
	Status       ApprovalStatus `json:"status"`
	Actor        string         `json:"actor,omitempty"`
	ExpiresAt    time.Time      `json:"expires_at"`
	DecidedAt    *time.Time     `json:"decided_at,omitempty"`
}

type Execution struct {
	ID             ExecutionID     `json:"id"`
	ActionID       ActionID        `json:"action_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Status         ExecutionStatus `json:"status"`
	Attempt        int             `json:"attempt"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

type Verification struct {
	ID          VerificationID  `json:"id"`
	ExecutionID ExecutionID     `json:"execution_id"`
	Succeeded   bool            `json:"succeeded"`
	Details     json.RawMessage `json:"details"`
	VerifiedAt  time.Time       `json:"verified_at"`
}
