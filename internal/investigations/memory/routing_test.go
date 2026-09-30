package memory

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

func receiveRoutingCase(t *testing.T, store *Store) (context.Context, domain.InvestigationID, time.Time) {
	t.Helper()
	ctx, now := context.Background(), time.Now().UTC()
	id, ticketID := domain.NewInvestigationID(), domain.NewTicketID()
	_, created, err := store.ReceiveTicket(ctx,
		domain.Ticket{ID: ticketID, Source: "jira", ExternalID: string(ticketID), Summary: "VPN trouble", CreatedAt: now},
		domain.Investigation{ID: id, TicketID: ticketID, Status: domain.InvestigationPending, CreatedAt: now})
	if err != nil || !created {
		t.Fatalf("receive case: created=%v err=%v", created, err)
	}
	return ctx, id, now
}

func TestDispatchPreservesOriginalDecisionAndIsolation(t *testing.T) {
	store := NewStore()
	ctx, id, now := receiveRoutingCase(t, store)
	confidence := 0.9
	decision := domain.RoutingDecision{
		Specialist: "help_desk", Classification: "connectivity", Source: domain.RoutingSourceModel,
		Reason: "VPN connectivity failure", Confidence: &confidence, ModelProvider: "local", Model: "test",
	}
	if err := store.Dispatch(ctx, id, decision, now); err != nil {
		t.Fatal(err)
	}
	confidence = 0.1
	item, err := store.GetInvestigation(ctx, id)
	if err != nil || item.Routing == nil || *item.Routing.Confidence != 0.9 {
		t.Fatalf("input alias changed routing: %+v err=%v", item, err)
	}
	*item.Routing.Confidence = 0.2
	item.Routing.Reason = "mutated"
	item, _ = store.GetInvestigation(ctx, id)
	if *item.Routing.Confidence != 0.9 || item.Routing.Reason != "VPN connectivity failure" {
		t.Fatal("returned routing aliases stored routing")
	}
	if err := store.StartInvestigation(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Handoff(ctx, id, "help_desk", "access_management", "Identity investigation needed", now); err != nil {
		t.Fatal(err)
	}
	item, _ = store.GetInvestigation(ctx, id)
	if item.Specialist != "access_management" || item.Routing.Specialist != "help_desk" {
		t.Fatalf("handoff changed original routing: %+v", item)
	}
	original := *item.Routing
	if err := store.Dispatch(ctx, id, original, now); err != nil {
		t.Fatalf("exact retry after handoff: %v", err)
	}
	conflicting := original
	conflicting.Reason = "different reason"
	if err := store.Dispatch(ctx, id, conflicting, now); !errors.Is(err, investigations.ErrInvalidTransition) {
		t.Fatalf("conflicting reason retry: %v", err)
	}
	events, err := store.Timeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	routed := 0
	for _, event := range events {
		if event.Type == audit.DispatcherRouted {
			routed++
			var recorded domain.RoutingDecision
			if err := json.Unmarshal(event.Data, &recorded); err != nil || !recorded.Equal(original) {
				t.Fatalf("audit routing = %+v err=%v", recorded, err)
			}
		}
	}
	if routed != 1 {
		t.Fatalf("routing audit count %d", routed)
	}
}

func TestDispatchConcurrentDecisionConflict(t *testing.T) {
	store := NewStore()
	ctx, id, now := receiveRoutingCase(t, store)
	decisions := []domain.RoutingDecision{
		{Specialist: "help_desk", Classification: "help_desk", Source: domain.RoutingSourceRule, RuleName: "rule_one", Reason: "Matched rule one"},
		{Specialist: "help_desk", Classification: "help_desk", Source: domain.RoutingSourceRule, RuleName: "rule_two", Reason: "Matched rule two"},
	}
	const callers = 24
	start, results := make(chan struct{}), make(chan error, callers)
	var group sync.WaitGroup
	for i := range callers {
		group.Add(1)
		go func(decision domain.RoutingDecision) {
			defer group.Done()
			<-start
			results <- store.Dispatch(ctx, id, decision, now)
		}(decisions[i%2])
	}
	close(start)
	group.Wait()
	close(results)
	successes, conflicts := 0, 0
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
	events, _ := store.Timeline(ctx, id)
	if len(events) != 2 || events[1].Type != audit.DispatcherRouted {
		t.Fatalf("concurrent retries duplicated audit: %+v", events)
	}
}

func TestInvalidDispatchHasNoSideEffects(t *testing.T) {
	store := NewStore()
	ctx, id, now := receiveRoutingCase(t, store)
	if err := store.Dispatch(ctx, id, domain.RoutingDecision{Specialist: "help_desk"}, now); !errors.Is(err, investigations.ErrInvalidTransition) {
		t.Fatalf("invalid dispatch: %v", err)
	}
	item, _ := store.GetInvestigation(ctx, id)
	events, _ := store.Timeline(ctx, id)
	if item.Specialist != "" || item.Routing != nil || len(events) != 1 {
		t.Fatal("invalid dispatch wrote state or audit")
	}
}
