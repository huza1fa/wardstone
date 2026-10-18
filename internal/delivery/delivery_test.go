package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

func TestDispatcherCompletesSuccessfulDelivery(t *testing.T) {
	t.Parallel()
	store := &fakeStore{job: testJob()}
	dispatcher, err := NewDispatcher(store, senderFunc(func(context.Context, domain.Ticket, domain.CaseMessage) (string, error) { return "10001", nil }), slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Lease: time.Second, PollInterval: time.Second, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.runOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.completedRemoteID != "10001" || store.retried || store.failed {
		t.Fatalf("store = %+v", store)
	}
}

func TestDispatcherRetriesOnlyRetryableErrors(t *testing.T) {
	t.Parallel()
	store := &fakeStore{job: testJob()}
	dispatcher, err := NewDispatcher(store, senderFunc(func(context.Context, domain.Ticket, domain.CaseMessage) (string, error) {
		return "", testSendError{retry: true}
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Lease: time.Second, PollInterval: time.Second, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.runOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.retried || store.failed {
		t.Fatalf("store = %+v", store)
	}
}

func TestDispatcherTreatsUntypedFailureAsUncertain(t *testing.T) {
	t.Parallel()
	store := &fakeStore{job: testJob()}
	dispatcher, err := NewDispatcher(store, senderFunc(func(context.Context, domain.Ticket, domain.CaseMessage) (string, error) {
		return "", errors.New("forbidden")
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Lease: time.Second, PollInterval: time.Second, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.runOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.failed || store.retried || store.failureStatus != domain.DeliveryUncertain {
		t.Fatalf("store = %+v", store)
	}
}

type fakeStore struct {
	job               Job
	completedRemoteID string
	retried           bool
	failed            bool
	failureStatus     domain.DeliveryStatus
}

func (s *fakeStore) ClaimDelivery(context.Context, string, time.Duration) (Job, error) {
	return s.job, nil
}
func (s *fakeStore) CompleteDelivery(_ context.Context, _ Job, remoteID string, _ time.Time) error {
	s.completedRemoteID = remoteID
	return nil
}
func (s *fakeStore) RetryDelivery(context.Context, Job, string, time.Time, time.Time) error {
	s.retried = true
	return nil
}
func (s *fakeStore) FailDelivery(_ context.Context, _ Job, _ string, status domain.DeliveryStatus, _ time.Time) error {
	s.failureStatus = status
	s.failed = true
	return nil
}

type senderFunc func(context.Context, domain.Ticket, domain.CaseMessage) (string, error)

func (f senderFunc) SendRequesterMessage(ctx context.Context, ticket domain.Ticket, message domain.CaseMessage) (string, error) {
	return f(ctx, ticket, message)
}

type testSendError struct{ retry bool }

func (e testSendError) Error() string   { return "transient" }
func (e testSendError) Retryable() bool { return e.retry }

func testJob() Job {
	return Job{Delivery: domain.MessageDelivery{ID: "dly_1", MessageID: "msg_1", Attempt: 1, LeaseOwner: "owner"}, Message: domain.CaseMessage{ID: "msg_1", InvestigationID: "inv_1", Source: "jira", Direction: domain.MessageOutbound, Body: "Question"}, Ticket: domain.Ticket{Source: "jira", ExternalID: "HELP-42"}}
}

type outcomeError struct{ unknown bool }

func (e outcomeError) Error() string        { return "fixture send error" }
func (e outcomeError) OutcomeUnknown() bool { return e.unknown }

func TestDispatcherSeparatesDefiniteAndUncertainOutcomes(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		store := &fakeStore{job: testJob()}
		d, err := NewDispatcher(store, senderFunc(func(context.Context, domain.Ticket, domain.CaseMessage) (string, error) {
			return "", outcomeError{unknown: unknown}
		}), slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Lease: time.Second, PollInterval: time.Second, MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.runOne(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := domain.DeliveryFailed
		if unknown {
			want = domain.DeliveryUncertain
		}
		if store.failureStatus != want || store.retried {
			t.Fatalf("outcome status=%s want=%s", store.failureStatus, want)
		}
	}
}
