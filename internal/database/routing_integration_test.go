package database

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
	"github.com/wardstone-project/wardstone/internal/worker"
)

func TestRoutingDecisionPostgres(t *testing.T) {
	databaseURL := os.Getenv("WARDSTONE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WARDSTONE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, now := NewStore(pool), time.Now().UTC()
	newCase := func(t *testing.T) domain.InvestigationID {
		t.Helper()
		id, ticketID := domain.NewInvestigationID(), domain.NewTicketID()
		_, created, err := store.ReceiveTicket(ctx,
			domain.Ticket{ID: ticketID, Source: "jira", ExternalID: "ROUTING-" + string(ticketID), Summary: "VPN issue", CreatedAt: now},
			domain.Investigation{ID: id, TicketID: ticketID, Status: domain.InvestigationPending, PromptVersion: "test", CreatedAt: now})
		if err != nil || !created {
			t.Fatalf("receive ticket: created=%v err=%v", created, err)
		}
		// Leave immutable audit evidence intact; make integration jobs unavailable
		// to later worker integration tests that share this database.
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `UPDATE jobs SET status = 'COMPLETED', completed_at = now(), lease_owner = NULL, lease_expires_at = NULL WHERE investigation_id = $1`, id)
		})
		return id
	}
	assertDecision := func(t *testing.T, id domain.InvestigationID, decision domain.RoutingDecision) {
		t.Helper()
		item, err := store.GetInvestigation(ctx, id)
		if err != nil || item.Routing == nil || !item.Routing.Equal(decision) {
			t.Fatalf("routing roundtrip = %+v, err=%v", item.Routing, err)
		}
		items, err := store.ListInvestigations(ctx, 10000)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, summary := range items {
			if summary.ID == id {
				found = true
				if summary.Routing == nil || !summary.Routing.Equal(decision) {
					t.Fatalf("admin routing roundtrip = %+v", summary.Routing)
				}
			}
		}
		if !found {
			t.Fatal("case missing from admin list")
		}
		events, err := store.Timeline(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		routed := 0
		for _, event := range events {
			if event.Type != audit.DispatcherRouted {
				continue
			}
			routed++
			var recorded domain.RoutingDecision
			if err := json.Unmarshal(event.Data, &recorded); err != nil || !recorded.Equal(decision) {
				t.Fatalf("audit routing = %+v err=%v", recorded, err)
			}
		}
		if routed != 1 {
			t.Fatalf("routed audit count = %d", routed)
		}
	}
	confidence := 0.8
	for _, decision := range []domain.RoutingDecision{
		{Specialist: "help_desk", Classification: "connectivity", Source: domain.RoutingSourceRule, RuleName: "vpn", Reason: "Matched VPN component"},
		{Specialist: "help_desk", Classification: "connectivity", Source: domain.RoutingSourceModel, Reason: "VPN issue", Confidence: &confidence, ModelProvider: "local", Model: "test"},
		{Specialist: "help_desk", Classification: "unclassified", Source: domain.RoutingSourceFallback, Reason: "Insufficient intent confidence", FallbackCode: "low_confidence", Confidence: &confidence, ModelProvider: "local", Model: "test"},
	} {
		t.Run(string(decision.Source), func(t *testing.T) {
			id := newCase(t)
			legacy, err := store.GetInvestigation(ctx, id)
			if err != nil || legacy.Routing != nil {
				t.Fatalf("undispatched routing should be nil: %+v err=%v", legacy, err)
			}
			if err := store.Dispatch(ctx, id, decision, now); err != nil {
				t.Fatal(err)
			}
			if err := store.Dispatch(ctx, id, decision, now.Add(time.Second)); err != nil {
				t.Fatalf("exact retry: %v", err)
			}
			conflict := decision
			conflict.Reason += " different"
			if err := store.Dispatch(ctx, id, conflict, now); !errors.Is(err, investigations.ErrInvalidTransition) {
				t.Fatalf("conflicting same-specialist route: %v", err)
			}
			if err := store.StartInvestigation(ctx, id, now); err != nil {
				t.Fatal(err)
			}
			if err := store.Handoff(ctx, id, "help_desk", "access_management", "Identity evidence needed", now); err != nil {
				t.Fatal(err)
			}
			if err := store.Dispatch(ctx, id, decision, now); err != nil {
				t.Fatalf("original route retry after handoff: %v", err)
			}
			item, _ := store.GetInvestigation(ctx, id)
			if item.Specialist != "access_management" {
				t.Fatalf("handoff overwritten: %+v", item)
			}
			assertDecision(t, id, decision)
			if _, err := pool.Exec(ctx, `UPDATE investigations SET routing = NULL WHERE id = $1`, id); err == nil {
				t.Fatal("database allowed erasing original routing")
			}
			assertDecision(t, id, decision)
		})
	}
	t.Run("concurrent conflict", func(t *testing.T) {
		id := newCase(t)
		first := domain.RoutingDecision{Specialist: "help_desk", Classification: "connectivity", Source: domain.RoutingSourceRule, RuleName: "one", Reason: "First rule"}
		second := first
		second.RuleName, second.Reason = "two", "Second rule"
		const callers = 12
		start, results := make(chan struct{}), make(chan error, callers)
		var group sync.WaitGroup
		for i := range callers {
			decision := first
			if i%2 != 0 {
				decision = second
			}
			group.Add(1)
			go func(decision domain.RoutingDecision) {
				defer group.Done()
				<-start
				results <- store.Dispatch(ctx, id, decision, now)
			}(decision)
		}
		close(start)
		group.Wait()
		close(results)
		var successes, conflicts int
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, investigations.ErrInvalidTransition) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if successes != callers/2 || conflicts != callers/2 {
			t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
		}
		item, _ := store.GetInvestigation(ctx, id)
		assertDecision(t, id, *item.Routing)
	})
	t.Run("invalid decision has no writes", func(t *testing.T) {
		id := newCase(t)
		if err := store.Dispatch(ctx, id, domain.RoutingDecision{Specialist: "help_desk"}, now); !errors.Is(err, investigations.ErrInvalidTransition) {
			t.Fatalf("invalid dispatch: %v", err)
		}
		item, err := store.GetInvestigation(ctx, id)
		if err != nil || item.Specialist != "" || item.Routing != nil {
			t.Fatalf("invalid dispatch wrote state: %+v err=%v", item, err)
		}
		events, err := store.Timeline(ctx, id)
		if err != nil || len(events) != 1 || events[0].Type != audit.TicketReceived {
			t.Fatalf("invalid dispatch wrote audit: %+v err=%v", events, err)
		}
	})
	t.Run("lease expiry while waiting for case lock", func(t *testing.T) {
		id := newCase(t)
		var job worker.Job
		job.InvestigationID, job.LeaseOwner, job.Attempt = id, "routing-lease-test", 1
		if err := pool.QueryRow(ctx, `UPDATE jobs SET status = 'RUNNING', attempt = 1,
			lease_owner = $2, lease_expires_at = clock_timestamp() + interval '1 hour'
			WHERE investigation_id = $1 RETURNING id, kind`, id, job.LeaseOwner).Scan(&job.ID, &job.Kind); err != nil {
			t.Fatal(err)
		}
		lock, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Rollback(ctx) }()
		var blockerPID int32
		if err := lock.QueryRow(ctx, `SELECT pg_backend_pid() FROM investigations WHERE id = $1 FOR UPDATE`, id).Scan(&blockerPID); err != nil {
			t.Fatal(err)
		}
		decision := domain.RoutingDecision{Specialist: "help_desk", Classification: "connectivity", Source: domain.RoutingSourceRule, RuleName: "vpn", Reason: "Matched component"}
		done := make(chan error, 1)
		go func() { done <- store.Dispatch(worker.WithLease(ctx, job), id, decision, now) }()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		blocked := false
		for !blocked {
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			select {
			case <-deadline.C:
				t.Fatal("dispatch did not wait for case lock")
			case <-ticker.C:
			}
		}
		// Expire the lease after Dispatch's transaction began. PostgreSQL now()
		// still sees the earlier transaction timestamp, so the lease fence must
		// compare clock_timestamp() after acquiring the investigation row lock.
		if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at = clock_timestamp() WHERE id = $1`, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := lock.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, worker.ErrLeaseLost) {
				t.Fatalf("expired lease dispatch = %v, want lease lost", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		item, err := store.GetInvestigation(ctx, id)
		events, eventsErr := store.Timeline(ctx, id)
		if err != nil || eventsErr != nil || item.Routing != nil || item.Specialist != "" || len(events) != 1 {
			t.Fatalf("expired lease wrote routing: %+v events=%v errors=%v/%v", item, events, err, eventsErr)
		}
	})
}
