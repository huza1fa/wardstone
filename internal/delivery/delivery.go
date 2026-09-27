// Package delivery dispatches durable requester messages. It deliberately owns
// no model or policy authority: it only sends messages already committed by an
// investigation transaction.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var (
	ErrNoDelivery = errors.New("no delivery available")
	ErrLeaseLost  = errors.New("delivery lease lost")
)

type Job struct {
	Delivery domain.MessageDelivery
	Message  domain.CaseMessage
	Ticket   domain.Ticket
}

type Store interface {
	ClaimDelivery(context.Context, string, time.Duration) (Job, error)
	CompleteDelivery(context.Context, Job, string, time.Time) error
	RetryDelivery(context.Context, Job, string, time.Time, time.Time) error
	FailDelivery(context.Context, Job, string, time.Time) error
}

type Sender interface {
	SendRequesterMessage(context.Context, domain.Ticket, domain.CaseMessage) (string, error)
}

type RetryableError interface {
	error
	Retryable() bool
}

type Config struct {
	Lease        time.Duration
	PollInterval time.Duration
	MaxAttempts  int
}

type Dispatcher struct {
	store  Store
	sender Sender
	logger *slog.Logger
	config Config
	owner  string
}

func NewDispatcher(store Store, sender Sender, logger *slog.Logger, config Config) (*Dispatcher, error) {
	if store == nil || sender == nil || logger == nil {
		return nil, errors.New("delivery store, sender, and logger are required")
	}
	if config.Lease <= 0 || config.PollInterval <= 0 || config.MaxAttempts < 1 {
		return nil, errors.New("delivery lease, poll interval, and max attempts must be positive")
	}
	return &Dispatcher{store: store, sender: sender, logger: logger, config: config, owner: string(domain.NewDeliveryID())}, nil
}

func (d *Dispatcher) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		err := d.runOne(ctx)
		if errors.Is(err, ErrNoDelivery) {
			timer.Reset(d.config.PollInterval)
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			d.logger.Error("dispatch requester message", "error", err)
			timer.Reset(d.config.PollInterval)
			continue
		}
		timer.Reset(0)
	}
}

func (d *Dispatcher) runOne(ctx context.Context) error {
	job, err := d.store.ClaimDelivery(ctx, d.owner, d.config.Lease)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, d.config.Lease/2)
	remoteID, sendErr := d.sender.SendRequesterMessage(callCtx, job.Ticket, job.Message)
	cancel()
	now := time.Now().UTC()
	if sendErr == nil {
		return d.store.CompleteDelivery(ctx, job, remoteID, now)
	}
	if retryable(sendErr) && job.Delivery.Attempt < d.config.MaxAttempts {
		return d.store.RetryDelivery(ctx, job, sendErr.Error(), now, now.Add(retryDelay(job.Delivery.Attempt)))
	}
	if err := d.store.FailDelivery(ctx, job, sendErr.Error(), now); err != nil {
		return fmt.Errorf("mark delivery dead after %v: %w", sendErr, err)
	}
	return nil
}

func retryable(err error) bool {
	var candidate RetryableError
	return errors.As(err, &candidate) && candidate.Retryable()
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Second << min(attempt-1, 6)
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}
