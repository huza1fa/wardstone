package jira

import (
	"context"
	"errors"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const Name domain.ConnectorName = "jira"

type TicketPayload struct {
	ExternalID    string `json:"external_id"`
	Summary       string `json:"summary"`
	Description   string `json:"description"`
	ReporterEmail string `json:"reporter_email"`
}

func Normalize(payload TicketPayload, now time.Time) (domain.Ticket, error) {
	if payload.ExternalID == "" || payload.Summary == "" {
		return domain.Ticket{}, errors.New("external_id and summary are required")
	}
	return domain.Ticket{
		ID: domain.NewTicketID(), Source: Name, ExternalID: payload.ExternalID,
		Summary: payload.Summary, Description: payload.Description,
		ReporterEmail: payload.ReporterEmail, CreatedAt: now,
	}, nil
}

type Updater interface {
	AddInvestigationResult(context.Context, string, string) error
}
