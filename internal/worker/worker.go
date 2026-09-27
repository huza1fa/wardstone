package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

var ErrNoJob = errors.New("no job available")
var ErrLeaseLost = errors.New("job lease lost")

type Job struct {
	ID              domain.JobID
	Kind            string
	InvestigationID domain.InvestigationID
	Attempt         int
	LeaseOwner      string
}

type Queue interface {
	Claim(context.Context, string, time.Duration) (Job, error)
	Renew(context.Context, Job, time.Duration) error
	Complete(context.Context, Job) error
	Fail(context.Context, Job, string) error
}

type Handler interface {
	Run(context.Context, domain.InvestigationID) error
}

type Pool struct {
	queue        Queue
	handler      Handler
	logger       *slog.Logger
	workers      int
	lease        time.Duration
	pollInterval time.Duration
	poolID       string
}

func NewPool(queue Queue, handler Handler, logger *slog.Logger, workers int, lease, pollInterval time.Duration) (*Pool, error) {
	if queue == nil || handler == nil || logger == nil {
		return nil, errors.New("queue, handler, and logger are required")
	}
	if workers < 1 || lease <= 0 || pollInterval <= 0 {
		return nil, errors.New("workers, lease, and poll interval must be positive")
	}
	return &Pool{queue: queue, handler: handler, logger: logger, workers: workers, lease: lease, pollInterval: pollInterval, poolID: string(domain.NewJobID())}, nil
}

func (p *Pool) Run(ctx context.Context) error {
	var workers sync.WaitGroup
	workers.Add(p.workers)
	for i := 0; i < p.workers; i++ {
		go func(index int) {
			defer workers.Done()
			p.runWorker(ctx, fmt.Sprintf("%s-worker-%d", p.poolID, index+1))
		}(i)
	}
	<-ctx.Done()
	workers.Wait()
	return ctx.Err()
}

func (p *Pool) runWorker(ctx context.Context, name string) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		job, err := p.queue.Claim(ctx, name, p.lease)
		if errors.Is(err, ErrNoJob) {
			timer.Reset(p.pollInterval)
			continue
		}
		if err != nil {
			p.logger.Error("claim job", "worker", name, "error", err)
			timer.Reset(p.pollInterval)
			continue
		}
		job.LeaseOwner = name
		err = p.runJob(ctx, job)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			p.logger.Error("investigation failed", "investigation_id", job.InvestigationID, "job_id", job.ID, "error", err)
			if failErr := p.queue.Fail(context.WithoutCancel(ctx), job, err.Error()); failErr != nil && !errors.Is(failErr, ErrLeaseLost) {
				p.logger.Error("mark job failed", "job_id", job.ID, "error", failErr)
			}
		} else if err := p.queue.Complete(ctx, job); err != nil {
			p.logger.Error("complete job", "job_id", job.ID, "error", err)
		}
		timer.Reset(0)
	}
}

func (p *Pool) runJob(ctx context.Context, job Job) error {
	jobCtx, cancel := context.WithCancel(WithLease(ctx, job))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.handler.Run(jobCtx, job.InvestigationID)
	}()
	interval := p.lease / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case <-ticker.C:
			if err := p.queue.Renew(ctx, job, p.lease); err != nil {
				cancel()
				<-done
				return fmt.Errorf("renew lease: %w", err)
			}
		}
	}
}

type leaseContextKey struct{}

func WithLease(ctx context.Context, job Job) context.Context {
	return context.WithValue(ctx, leaseContextKey{}, job)
}

func LeaseFromContext(ctx context.Context) (Job, bool) {
	job, ok := ctx.Value(leaseContextKey{}).(Job)
	return job, ok
}
