package delivery

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var (
	ErrRecoveryConflict        = errors.New("delivery changed or cannot be recovered; refresh the investigation")
	ErrInvalidRecovery         = errors.New("recovery requires an action, revision, operator identity, and reason; resend requires duplicate-risk acknowledgement")
	ErrCommentMismatch         = errors.New("comment_mismatch")
	ErrCommentNotFound         = errors.New("comment_not_found")
	ErrVerificationUnavailable = errors.New("jira_verification_unavailable")
)

type RecoveryRequest struct {
	Action                   string `json:"action"`
	Revision                 *int64 `json:"revision"`
	Actor                    string `json:"actor"`
	Reason                   string `json:"reason"`
	RemoteID                 string `json:"remote_id,omitempty"`
	AcknowledgeDuplicateRisk bool   `json:"acknowledge_duplicate_risk,omitempty"`
}

func (r RecoveryRequest) Validate() error {
	validText := func(s string, limit int) bool {
		return strings.TrimSpace(s) != "" && len(s) <= limit && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
	}
	if r.Revision == nil || *r.Revision < 0 || !validText(r.Actor, 256) || !validText(r.Reason, 2000) {
		return ErrInvalidRecovery
	}
	switch r.Action {
	case "reconcile":
		if !ValidRemoteID(r.RemoteID) || r.AcknowledgeDuplicateRisk {
			return ErrInvalidRecovery
		}
	case "resend":
		if !r.AcknowledgeDuplicateRisk || r.RemoteID != "" {
			return ErrInvalidRecovery
		}
	default:
		return ErrInvalidRecovery
	}
	return nil
}

func ValidRemoteID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

type RecoveryStore interface {
	GetRecoveryJob(context.Context, domain.InvestigationID, domain.MessageID) (Job, error)
	RecordRecovery(context.Context, Job, RecoveryRequest, string, time.Time) (domain.MessageDelivery, error)
}

type CommentVerifier interface {
	VerifyRequesterMessage(context.Context, Job, string) error
}

type RecoveryService struct {
	store    RecoveryStore
	verifier CommentVerifier
}

func NewRecoveryService(store RecoveryStore, verifier CommentVerifier) (*RecoveryService, error) {
	if store == nil || verifier == nil {
		return nil, errors.New("recovery store and Jira verifier are required")
	}
	return &RecoveryService{store: store, verifier: verifier}, nil
}

type RecoveryResult struct {
	Delivery domain.MessageDelivery `json:"delivery"`
	Outcome  string                 `json:"outcome"`
}

// Recover performs bounded read-only remote verification outside transactions.
// The store rechecks the revision and case state when committing the decision.
func (s *RecoveryService) Recover(ctx context.Context, id domain.InvestigationID, mid domain.MessageID, r RecoveryRequest) (RecoveryResult, error) {
	if err := r.Validate(); err != nil {
		return RecoveryResult{}, err
	}
	job, err := s.store.GetRecoveryJob(ctx, id, mid)
	if err != nil {
		return RecoveryResult{}, err
	}
	if job.Delivery.ReviewedAt == nil || job.Ticket.Source != "jira" || job.Message.Source != "jira" || job.Message.Direction != domain.MessageOutbound || job.Delivery.Revision != *r.Revision ||
		(job.Delivery.Status != domain.DeliveryUncertain && job.Delivery.Status != domain.DeliveryFailed) {
		return RecoveryResult{}, ErrRecoveryConflict
	}
	outcome := "resend_queued"
	if r.Action == "reconcile" {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = s.verifier.VerifyRequesterMessage(checkCtx, job, r.RemoteID)
		cancel()
		switch {
		case err == nil:
			outcome = "verified"
		case errors.Is(err, ErrCommentNotFound):
			outcome = "comment_not_found"
		case errors.Is(err, ErrCommentMismatch):
			outcome = "comment_mismatch"
		default:
			outcome = "jira_verification_unavailable"
		}
	}
	d, err := s.store.RecordRecovery(ctx, job, r, outcome, time.Now().UTC())
	return RecoveryResult{Delivery: d, Outcome: outcome}, err
}
