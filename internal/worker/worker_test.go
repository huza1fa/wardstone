package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestRunJobRenewsLeaseUntilHandlerCompletes(t *testing.T) {
	t.Parallel()
	queue := &renewQueue{renewed: make(chan struct{}, 1)}
	release := make(chan struct{})
	handler := handlerFunc(func(ctx context.Context, _ domain.InvestigationID) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	pool, err := NewPool(queue, handler, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, 30*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- pool.runJob(context.Background(), Job{ID: "job_1", InvestigationID: "inv_1", Attempt: 1, LeaseOwner: "worker"})
	}()
	<-queue.renewed
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if queue.renewCalls.Load() == 0 {
		t.Fatal("lease was not renewed")
	}
}

func TestRunJobCancelsHandlerWhenLeaseIsLost(t *testing.T) {
	t.Parallel()
	queue := &renewQueue{renewed: make(chan struct{}, 1), renewErr: ErrLeaseLost}
	handlerStopped := make(chan struct{})
	handler := handlerFunc(func(ctx context.Context, _ domain.InvestigationID) error {
		<-ctx.Done()
		close(handlerStopped)
		return ctx.Err()
	})
	pool, err := NewPool(queue, handler, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, 30*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.runJob(context.Background(), Job{ID: "job_1", InvestigationID: "inv_1", Attempt: 1, LeaseOwner: "worker"})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("error = %v, want lease lost", err)
	}
	<-handlerStopped
}

type handlerFunc func(context.Context, domain.InvestigationID) error

func (f handlerFunc) Run(ctx context.Context, id domain.InvestigationID) error { return f(ctx, id) }

type renewQueue struct {
	renewCalls atomic.Int32
	renewed    chan struct{}
	renewErr   error
}

func (*renewQueue) Claim(context.Context, string, time.Duration) (Job, error) {
	return Job{}, ErrNoJob
}
func (q *renewQueue) Renew(context.Context, Job, time.Duration) error {
	q.renewCalls.Add(1)
	select {
	case q.renewed <- struct{}{}:
	default:
	}
	return q.renewErr
}
func (*renewQueue) Complete(context.Context, Job) error     { return nil }
func (*renewQueue) Fail(context.Context, Job, string) error { return nil }
