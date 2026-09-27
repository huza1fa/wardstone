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

func TestDispatcherMarksPermanentFailureDead(t *testing.T) {
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
	if !store.failed || store.retried {
		t.Fatalf("store = %+v", store)
	}
}

type fakeStore struct {
	job               Job
	completedRemoteID string
	retried           bool
	failed            bool
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
func (s *fakeStore) FailDelivery(context.Context, Job, string, time.Time) error {
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
