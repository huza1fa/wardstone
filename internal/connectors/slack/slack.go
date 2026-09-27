package slack

import (
	"context"

	"github.com/wardstone-project/wardstone/internal/domain"
)

const Name domain.ConnectorName = "slack"

// ApprovalNotifier sends an approval request but does not decide or execute it.
// Implementations must include the exact action digest in interactive metadata.
type ApprovalNotifier interface {
	RequestApproval(context.Context, domain.ProposedAction, domain.Approval) error
}
