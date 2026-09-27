package approvals

import (
	"errors"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var (
	ErrNotGranted = errors.New("approval is not granted")
	ErrExpired    = errors.New("approval has expired")
)

func Validate(approval domain.Approval, action domain.ProposedAction, now time.Time) error {
	if !now.Before(approval.ExpiresAt) {
		return ErrExpired
	}
	if approval.Status != domain.ApprovalGranted {
		return ErrNotGranted
	}
	if approval.ActionID != action.ID || approval.ActionDigest != action.Digest || !action.DigestValid() {
		return ErrActionChanged
	}
	return nil
}
