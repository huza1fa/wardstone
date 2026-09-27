package approvals

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const (
	maxApprovalLifetime = 7 * 24 * time.Hour
	maxActorLength      = 256
)

var (
	ErrNotFound          = errors.New("approval not found")
	ErrAlreadyDecided    = errors.New("approval has already been decided")
	ErrActionChanged     = errors.New("approved action has changed")
	ErrActionNotEligible = errors.New("action does not require approval")
	ErrInvalidDecision   = errors.New("approval decision must be GRANTED or DENIED")
	ErrInvalidActor      = errors.New("approval actor is required and must not exceed 256 characters")
)

// Store owns the transactional approval state transitions. Implementations
// must bind requests to a persisted action and serialize concurrent decisions.
type Store interface {
	CreateApproval(context.Context, domain.ProposedAction, domain.Approval, time.Time) (domain.Approval, bool, error)
	GetApproval(context.Context, domain.ApprovalID) (domain.Approval, error)
	DecideApproval(context.Context, domain.ApprovalID, domain.ApprovalStatus, string, time.Time) (domain.Approval, error)
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type Service struct {
	store    Store
	lifetime time.Duration
	clock    Clock
}

func NewService(store Store, lifetime time.Duration) (*Service, error) {
	if store == nil {
		return nil, errors.New("approval store is required")
	}
	if lifetime <= 0 || lifetime > maxApprovalLifetime {
		return nil, errors.New("approval lifetime must be positive and no greater than 7 days")
	}
	return &Service{store: store, lifetime: lifetime, clock: realClock{}}, nil
}

func (s *Service) SetClockForTest(clock Clock) {
	s.clock = clock
}

// Request creates at most one active approval for an action. The store
// independently verifies that the persisted action has this digest and a
// REQUIRE_APPROVAL policy decision.
func (s *Service) Request(ctx context.Context, action domain.ProposedAction) (domain.Approval, bool, error) {
	if action.ID == "" || action.InvestigationID == "" || !action.DigestValid() {
		return domain.Approval{}, false, ErrActionChanged
	}
	now := s.clock.Now()
	approval := domain.Approval{
		ID:           domain.NewApprovalID(),
		ActionID:     action.ID,
		ActionDigest: action.Digest,
		Status:       domain.ApprovalPending,
		ExpiresAt:    now.Add(s.lifetime),
	}
	return s.store.CreateApproval(ctx, action, approval, now)
}

func (s *Service) Get(ctx context.Context, id domain.ApprovalID) (domain.Approval, error) {
	if id == "" {
		return domain.Approval{}, ErrNotFound
	}
	return s.store.GetApproval(ctx, id)
}

// Decide permits only terminal operator decisions. The store performs the
// pending/expiry check atomically so simultaneous callbacks cannot both win.
func (s *Service) Decide(ctx context.Context, id domain.ApprovalID, decision domain.ApprovalStatus, actor string) (domain.Approval, error) {
	if id == "" {
		return domain.Approval{}, ErrNotFound
	}
	if err := ValidateDecisionInput(decision, actor); err != nil {
		return domain.Approval{}, err
	}
	return s.store.DecideApproval(ctx, id, decision, actor, s.clock.Now())
}

// ValidateDecisionInput is shared with store implementations so invalid state
// transitions are rejected even when a store is called without Service.
func ValidateDecisionInput(decision domain.ApprovalStatus, actor string) error {
	if decision != domain.ApprovalGranted && decision != domain.ApprovalDenied {
		return ErrInvalidDecision
	}
	if strings.TrimSpace(actor) == "" || len(actor) > maxActorLength {
		return ErrInvalidActor
	}
	return nil
}
