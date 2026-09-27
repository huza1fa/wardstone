package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

type TicketID string
type InvestigationID string
type EvidenceID string
type ActionID string
type ApprovalID string
type ExecutionID string
type VerificationID string
type JobID string
type ConnectorName string
type CapabilityName string
type ModelProviderName string

func NewTicketID() TicketID               { return TicketID(newID("tkt")) }
func NewInvestigationID() InvestigationID { return InvestigationID(newID("inv")) }
func NewEvidenceID() EvidenceID           { return EvidenceID(newID("evd")) }
func NewActionID() ActionID               { return ActionID(newID("act")) }
func NewApprovalID() ApprovalID           { return ApprovalID(newID("apr")) }
func NewExecutionID() ExecutionID         { return ExecutionID(newID("exe")) }
func NewVerificationID() VerificationID   { return VerificationID(newID("ver")) }
func NewJobID() JobID                     { return JobID(newID("job")) }

func newID(prefix string) string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(fmt.Sprintf("generate %s ID: %v", prefix, err))
	}
	return prefix + "_" + hex.EncodeToString(data[:])
}
