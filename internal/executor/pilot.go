package executor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/wardstone-project/wardstone/internal/cases"
	"github.com/wardstone-project/wardstone/internal/connectors/testgroup"
	"github.com/wardstone-project/wardstone/internal/domain"
)

var ErrNoExecution = errors.New("no test execution available")
var ErrLeaseLost = errors.New("test execution lease lost")
var ErrConflict = errors.New("test execution or approval is not eligible")

type Proposal struct {
	cases.Command
	Group  string `json:"group"`
	Member string `json:"member"`
}

func (p Proposal) Validate() error {
	if p.Command.Validate() != nil || !testgroup.ValidName(p.Group) || !testgroup.ValidName(p.Member) {
		return cases.ErrInvalid
	}
	return nil
}

type Job struct {
	Execution domain.Execution
	Action    domain.ProposedAction
	Owner     string
}
type Summary struct {
	Action       domain.ProposedAction `json:"action"`
	Approval     domain.Approval       `json:"approval"`
	Execution    *domain.Execution     `json:"execution,omitempty"`
	Verification *domain.Verification  `json:"verification,omitempty"`
}
type Store interface {
	ProposeTestAction(context.Context, Proposal, time.Duration, time.Time) (Summary, bool, error)
	QueueTestExecution(context.Context, domain.ApprovalID, cases.Command, time.Time) (domain.ExecutionID, error)
	RecheckTestExecution(context.Context, domain.ExecutionID, cases.Command, time.Time) error
	ListTestExecutions(context.Context) ([]Summary, error)
	ClaimExecution(context.Context, string, time.Duration) (Job, error)
	RecordMutation(context.Context, Job, json.RawMessage, time.Time) error
	FinishVerification(context.Context, Job, domain.Verification, time.Time) error
	FailExecution(context.Context, Job, string, string, time.Time) error
}

// Pilot deliberately supports exactly one synthetic capability. No real
// connector mutator is installed, and SHADOW/REVIEW infrastructure policy stays
// unchanged. Multiple instances coordinate through durable leases and receipts.
type Pilot struct {
	store    Store
	mutator  Mutator
	verifier Verifier
	lease    time.Duration
}

func NewPilot(s Store, m Mutator, v Verifier, lease time.Duration) (*Pilot, error) {
	if s == nil || m == nil || v == nil || lease < time.Second {
		return nil, errors.New("test execution store, connector, verifier, and a lease >=1s are required")
	}
	return &Pilot{s, m, v, lease}, nil
}
func (p *Pilot) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	owner := string(domain.NewJobID())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		job, err := p.store.ClaimExecution(ctx, owner, p.lease)
		if err == nil {
			if runErr := p.RunJob(ctx, job); runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, ErrLeaseLost) {
				slog.Error("test execution job failed", "execution_id", job.Execution.ID, "error", runErr)
			}
		} else if !errors.Is(err, ErrNoExecution) && !errors.Is(err, context.Canceled) {
			slog.Error("claim test execution", "error", err)
		}
		// Claims and operation errors leave durable states for bounded recovery.
		timer.Reset(500 * time.Millisecond)
	}
}
func (p *Pilot) RunJob(ctx context.Context, job Job) error {
	if job.Action.Capability != testgroup.AddMember || !job.Action.DigestValid() {
		return p.store.FailExecution(ctx, job, "FAILED", "invalid_test_action", time.Now().UTC())
	}
	// Stop before the lease can expire. Recovered mutations use the same durable
	// idempotency key; recovered VERIFYING jobs only read the independent state.
	callCtx, cancel := context.WithTimeout(ctx, p.lease*2/3)
	defer cancel()
	if job.Execution.Status == domain.ExecutionRunning {
		result, err := p.mutator.Execute(callCtx, job.Action.Capability, job.Action.Arguments, job.Execution.IdempotencyKey)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			status, code := "NEEDS_RECONCILIATION", "mutation_outcome_unknown"
			if errors.Is(err, testgroup.ErrStale) || errors.Is(err, testgroup.ErrInvalid) {
				status, code = "FAILED", "test_precondition_changed"
			}
			return p.store.FailExecution(ctx, job, status, code, time.Now().UTC())
		}
		if err := p.store.RecordMutation(callCtx, job, result, time.Now().UTC()); err != nil {
			return err
		}
		job.Execution.Status = domain.ExecutionStatus("VERIFYING")
	}
	verification, err := p.verifier.Verify(callCtx, job.Action, job.Execution)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return p.store.FailExecution(ctx, job, "NEEDS_RECONCILIATION", "verification_unavailable", time.Now().UTC())
	}
	return p.store.FinishVerification(callCtx, job, verification, time.Now().UTC())
}
