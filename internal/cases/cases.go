// Package cases defines authenticated operator commands. A case is the original
// investigation plus an append-only chain of fresh investigation attempts.
package cases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var ErrConflict = errors.New("case command conflicts with current state or request ID")
var ErrInvalid = errors.New("invalid case command")
var requestID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

type Command struct {
	RequestID string `json:"request_id"`
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
}

func (c Command) Validate() error {
	if !requestID.MatchString(c.RequestID) || !text(c.Actor, 256) || !text(c.Reason, 1000) {
		return ErrInvalid
	}
	return nil
}
func text(s string, n int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

type Create struct {
	Command
	Summary     string                `json:"summary"`
	Description string                `json:"description"`
	Metadata    domain.TicketMetadata `json:"metadata"`
}

func (c Create) Validate() error {
	if err := c.Command.Validate(); err != nil {
		return err
	}
	t := domain.Ticket{ID: "validation", Source: "operator", ExternalID: c.RequestID, Summary: c.Summary, Description: c.Description, Metadata: c.Metadata}
	if t.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

type Reply struct {
	Command
	Body string `json:"body"`
}

func (r Reply) Validate() error {
	if r.Command.Validate() != nil || !text(r.Body, 16000) {
		return ErrInvalid
	}
	return nil
}
func Digest(v any) string {
	data, _ := json.Marshal(v)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

type Store interface {
	GetTicket(context.Context, domain.InvestigationID) (domain.Ticket, error)
	CreateCase(context.Context, Create, string, time.Time) (domain.InvestigationID, bool, error)
	CancelCase(context.Context, domain.InvestigationID, Command, time.Time) error
	RetryCase(context.Context, domain.InvestigationID, Command, string, time.Time) (domain.InvestigationID, bool, error)
	ReplyToCase(context.Context, domain.InvestigationID, Reply, time.Time) (bool, error)
	CaseAttempts(context.Context, domain.InvestigationID) ([]domain.Investigation, error)
}
