package jira

import (
	"context"
	"errors"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const Name domain.ConnectorName = "jira"

type TicketPayload struct {
	ExternalID    string   `json:"external_id"`
	Summary       string   `json:"summary"`
	Description   string   `json:"description"`
	ReporterEmail string   `json:"reporter_email"`
	IssueType     string   `json:"issue_type"`
	RequestType   string   `json:"request_type"`
	Components    []string `json:"components"`
	Labels        []string `json:"labels"`
	// Fields is the allowlisted, flattened subset of Jira custom fields that
	// the automation sends to Wardstone. Nested Jira payloads must be flattened
	// by the connector boundary rather than passed to the model unexamined.
	Fields map[string][]string `json:"fields"`
}

type ReplyPayload struct {
	ExternalID string `json:"external_id"`
	CommentID  string `json:"comment_id"`
	Author     string `json:"author"`
	Body       string `json:"body"`
}

func NormalizeReply(payload ReplyPayload, now time.Time) (string, domain.CaseMessage, error) {
	if payload.ExternalID == "" || payload.CommentID == "" || payload.Author == "" || payload.Body == "" {
		return "", domain.CaseMessage{}, errors.New("external_id, comment_id, author, and body are required")
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
		ReporterEmail: payload.ReporterEmail,
		Metadata: domain.TicketMetadata{
			IssueType: payload.IssueType, RequestType: payload.RequestType,
			Components: append([]string(nil), payload.Components...),
			Labels:     append([]string(nil), payload.Labels...), Fields: cloneFields(payload.Fields),
		},
		CreatedAt: now,
	}, nil
}

func cloneFields(fields map[string][]string) map[string][]string {
	if len(fields) == 0 {
		return nil
	}
	clone := make(map[string][]string, len(fields))
	for key, values := range fields {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

type Updater interface {
	AddInvestigationResult(context.Context, string, string) error
}
