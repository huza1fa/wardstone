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

type ReplyPayload struct {
	ExternalID string `json:"external_id"`
	CommentID  string `json:"comment_id"`
	Author     string `json:"author"`
	Body       string `json:"body"`
}

func NormalizeReply(payload ReplyPayload, now time.Time) (string, domain.CaseMessage, error) {
	if payload.ExternalID == "" || payload.CommentID == "" || payload.Body == "" {
		return "", domain.CaseMessage{}, errors.New("external_id, comment_id, and body are required")
	}
	return payload.ExternalID, domain.CaseMessage{
		ID: domain.NewMessageID(), Source: Name, ExternalID: payload.CommentID,
		Direction: domain.MessageInbound, Author: payload.Author, Body: payload.Body, CreatedAt: now,
	}, nil
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
