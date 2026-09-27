package approvals

import (
	"errors"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var (
	ErrNotGranted    = errors.New("approval is not granted")
	ErrExpired       = errors.New("approval has expired")
	ErrActionChanged = errors.New("approved action has changed")
)

func Validate(approval domain.Approval, action domain.ProposedAction, now time.Time) error {
	if approval.Status != domain.ApprovalGranted {
		return ErrNotGranted
	}
	if !now.Before(approval.ExpiresAt) {
		return ErrExpired
	}
	if approval.ActionID != action.ID || approval.ActionDigest != action.Digest || !action.DigestValid() {
		return ErrActionChanged
	}
	return nil
}
