// Package admin defines read models shared by Wardstone's operator interfaces.
// It deliberately contains no transport or persistence concerns so the web UI,
// terminal UI, and database adapter can share one stable contract.
package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

type InvestigationCounts struct {
	Total     int64 `json:"total"`
	Pending   int64 `json:"pending"`
	Running   int64 `json:"running"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
}

type ApprovalCounts struct {
	Total   int64 `json:"total"`
	Pending int64 `json:"pending"`
	Granted int64 `json:"granted"`
	Denied  int64 `json:"denied"`
	Expired int64 `json:"expired"`
}

type JobCounts struct {
	Pending   int64 `json:"pending"`
	Running   int64 `json:"running"`
	Completed int64 `json:"completed"`
	Dead      int64 `json:"dead"`
}

type Overview struct {
	Status         string               `json:"status"`
	Mode           domain.OperatingMode `json:"mode"`
	GeneratedAt    time.Time            `json:"generated_at"`
	Investigations InvestigationCounts  `json:"investigations"`
	Approvals      ApprovalCounts       `json:"approvals"`
	Jobs           JobCounts            `json:"jobs"`
}

type TicketSummary struct {
	Source        domain.ConnectorName `json:"source"`
	ExternalID    string               `json:"external_id"`
	Summary       string               `json:"summary"`
	ReporterEmail string               `json:"reporter_email,omitempty"`
}

type InvestigationSummary struct {
	domain.Investigation
	Ticket TicketSummary `json:"ticket"`
}

type ApprovalSummary struct {
	domain.Approval
	InvestigationID domain.InvestigationID `json:"investigation_id"`
	Capability      domain.CapabilityName  `json:"capability"`
	Arguments       json.RawMessage        `json:"arguments"`
	Reason          string                 `json:"reason"`
	PolicyReason    string                 `json:"policy_reason"`
	CreatedAt       time.Time              `json:"created_at"`
	Ticket          TicketSummary          `json:"ticket"`
}

type Reader interface {
	Overview(context.Context) (Overview, error)
	ListInvestigations(context.Context, int) ([]InvestigationSummary, error)
	ListApprovals(context.Context, domain.ApprovalStatus) ([]ApprovalSummary, error)
}
