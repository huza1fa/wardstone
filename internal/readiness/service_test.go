package readiness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/connectors"
)

func TestStatusPreservesOrderAndReportsRequiredSetup(t *testing.T) {
	t.Parallel()
	service := newTestService(t, []Check{
		{Name: "database", Label: "Database", Description: "Storage", Permission: "Database access", Required: true, Configured: true, Ready: true, ReadyCode: "ready", ReadyMessage: "Ready."},
		{Name: "model", Label: "Model", Description: "Reasoning", Permission: "Model metadata", Required: true},
	})
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != admin.SetupRequired || len(status.Components) != 2 || status.Components[0].Name != "database" || status.Components[1].Name != "model" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestProbeReturnsSanitizedFailureAndCachesIt(t *testing.T) {
	t.Parallel()
	secret := "https://secret-user:secret-token@example.test/private"
	var logs bytes.Buffer
	service, err := New([]Check{{
		Name: "model", Label: "Model", Description: "Reasoning", Permission: "Metadata", Required: true, Configured: true,
		Probe: func(context.Context) error { return errors.New(secret) },
	}}, admin.RuntimeFeatures{}, 20*time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Probe(context.Background(), "model")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != admin.SetupFailed || result.Code != "unavailable" || strings.Contains(result.Message, secret) {
		t.Fatalf("unsafe result: %+v", result)
	}
	status, _ := service.Status(context.Background())
	if status.State != admin.SetupDegraded || status.Components[0].Code != "unavailable" {
		t.Fatalf("failure was not cached: %+v", status)
	}
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), "component=model") || !strings.Contains(logs.String(), "result=unavailable") {
		t.Fatalf("unsafe or incomplete probe log: %q", logs.String())
	}
}

func TestProbeMapsStableFailureAndTimeout(t *testing.T) {
	t.Parallel()
	service := newTestService(t, []Check{
		{Name: "jira", Label: "Jira", Description: "Tickets", Permission: "Read", Configured: true, Probe: func(context.Context) error { return connectors.NewProbeError(connectors.ProbeForbidden) }},
		{Name: "slow", Label: "Slow", Description: "Slow service", Permission: "Read", Configured: true, Probe: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
	})
	result, err := service.Probe(context.Background(), "jira")
	if err != nil || result.Code != "forbidden" || !strings.Contains(result.Message, "permission") {
		t.Fatalf("forbidden result=%+v err=%v", result, err)
	}
	result, err = service.Probe(context.Background(), "slow")
	if err != nil || result.Code != "timeout" {
		t.Fatalf("timeout result=%+v err=%v", result, err)
	}
}

func TestRequiredMissingTakesPrecedenceOverOptionalFailure(t *testing.T) {
	t.Parallel()
	service := newTestService(t, []Check{
		{Name: "model", Label: "Model", Description: "Reasoning", Permission: "Metadata", Required: true},
		{Name: "google", Label: "Google", Description: "Evidence", Permission: "Directory read", Configured: true, Probe: func(context.Context) error { return connectors.NewProbeError(connectors.ProbeForbidden) }},
	})
	if _, err := service.Probe(context.Background(), "google"); err != nil {
		t.Fatal(err)
	}
	status, _ := service.Status(context.Background())
	if status.State != admin.SetupRequired {
		t.Fatalf("state = %q, want setup_required", status.State)
	}
}

func TestProbeRejectsInvalidAndConcurrentRequests(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	service := newTestService(t, []Check{
		{Name: "active", Label: "Active", Description: "Active", Permission: "Read", Configured: true, Probe: func(context.Context) error { close(started); <-release; return nil }},
		{Name: "missing", Label: "Missing", Description: "Missing", Permission: "Read"},
		{Name: "static", Label: "Static", Description: "Static", Permission: "Read", Configured: true, Ready: true, ReadyCode: "configured", ReadyMessage: "Configured."},
	})
	done := make(chan error, 1)
	go func() { _, err := service.Probe(context.Background(), "active"); done <- err }()
	<-started
	if _, err := service.Probe(context.Background(), "active"); !errors.Is(err, ErrProbeRunning) {
		t.Fatalf("concurrent error = %v", err)
	}
	if _, err := service.Probe(context.Background(), "missing"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured error = %v", err)
	}
	if _, err := service.Probe(context.Background(), "static"); !errors.Is(err, ErrNotProbeable) {
		t.Fatalf("static error = %v", err)
	}
	if _, err := service.Probe(context.Background(), "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Probe(context.Background(), "active"); !errors.Is(err, ErrProbeThrottled) {
		t.Fatalf("throttled error = %v", err)
	}
}

func newTestService(t *testing.T, checks []Check) *Service {
	t.Helper()
	service, err := New(checks, admin.RuntimeFeatures{}, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}
