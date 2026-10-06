package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/audit"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/investigations"
)

func TestCaseMemoryPostgres(t *testing.T) {
	databaseURL := os.Getenv("WARDSTONE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WARDSTONE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, now := NewStore(pool), time.Now().UTC().Truncate(time.Microsecond)
	newCase := func(t *testing.T) domain.InvestigationID {
		t.Helper()
		id, ticketID := domain.NewInvestigationID(), domain.NewTicketID()
		_, created, err := store.ReceiveTicket(ctx,
			domain.Ticket{ID: ticketID, Source: "jira", ExternalID: "MEMORY-" + string(ticketID), Summary: "Access issue", CreatedAt: now},
			domain.Investigation{ID: id, TicketID: ticketID, Status: domain.InvestigationPending, PromptVersion: "test", CreatedAt: now})
		if err != nil || !created {
			t.Fatalf("receive ticket: created=%v err=%v", created, err)
		}
		// Preserve append-only audit rows, but prevent interference with worker tests.
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `UPDATE jobs SET status = 'COMPLETED', completed_at = now(), lease_owner = NULL, lease_expires_at = NULL WHERE investigation_id = $1`, id)
		})
		if err := store.StartInvestigation(ctx, id, now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	evidence := func(id domain.EvidenceID, capability domain.CapabilityName, at time.Time) domain.Evidence {
		return domain.Evidence{ID: id, Source: "google", Capability: capability, Kind: "membership", Summary: "Membership observation", Data: json.RawMessage(`{"member":false}`), ObservedAt: at}
	}
	report := func(evidenceID domain.EvidenceID) *domain.WorkPackage {
		return &domain.WorkPackage{Version: 1, Summary: "Group membership is absent",
			Findings: []domain.Finding{{Kind: "verified", Statement: "Requester is not a group member", EvidenceIDs: []domain.EvidenceID{evidenceID}}},
			Unknowns: []string{"Whether access is still required"}, EscalationReason: "Owner authorization required",
			RequesterSummary: "Access request is ready for review", ExpectedImpact: "Restore group access"}
	}
	evaluation := func(id domain.InvestigationID, evidenceID domain.EvidenceID) investigations.ActionEvaluation {
		action := domain.ProposedAction{ID: domain.NewActionID(), InvestigationID: id, Capability: "google.groups.add_member",
			Arguments: json.RawMessage(`{"group":"team@example.com","user":"requester@example.com"}`),
			Reason:    "Restore access", EvidenceIDs: []domain.EvidenceID{evidenceID}, CreatedAt: now}
		if err := action.Seal(); err != nil {
			t.Fatal(err)
		}
		return investigations.ActionEvaluation{Action: action, Policy: domain.PolicyResult{Decision: domain.PolicyRequireApproval, Reason: "Owner review"}}
	}
	message := func() *domain.CaseMessage {
		id := domain.NewMessageID()
		return &domain.CaseMessage{ID: id, Source: "jira", ExternalID: string(id), Direction: domain.MessageOutbound, Author: "wardstone", Body: "Ready for owner review", CreatedAt: now}
	}

	t.Run("provenance before limit and deterministic order", func(t *testing.T) {
		id := newCase(t)
		prefix := string(domain.NewEvidenceID())
		older, first, second := domain.EvidenceID(prefix+"_old"), domain.EvidenceID(prefix+"_a"), domain.EvidenceID(prefix+"_b")
		items := []domain.Evidence{evidence(first, "google.groups.read", now), evidence(older, "google.groups.read", now.Add(-time.Hour)), evidence(second, "google.groups.read", now)}
		// More disallowed observations than the maximum result bound, all newer.
		for i := range 129 {
			items = append(items, evidence(domain.NewEvidenceID(), "google.directory.read", now.Add(time.Hour+time.Duration(i)*time.Second)))
		}
		if err := store.RecordEvidence(ctx, id, items, []investigations.CollectionFailure{{Source: "google", Capability: "google.groups.read", Error: "timeout", Duration: time.Second}}, now); err != nil {
			t.Fatal(err)
		}
		// This is exactly how a pre-migration row looks: identical source/kind,
		// but no capability. Its default must remain unknown and invisible.
		legacyID := domain.NewEvidenceID()
		if _, err := pool.Exec(ctx, `INSERT INTO evidence(id, investigation_id, source, kind, summary, data, observed_at)
			VALUES ($1, $2, 'google', 'membership', 'legacy', '{}', $3)`, legacyID, id, now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var legacyCapability string
		if err := pool.QueryRow(ctx, `SELECT capability FROM evidence WHERE id = $1`, legacyID).Scan(&legacyCapability); err != nil || legacyCapability != "" {
			t.Fatalf("legacy provenance = %q err=%v", legacyCapability, err)
		}
		otherID := newCase(t)
		if err := store.RecordEvidence(ctx, otherID, []domain.Evidence{evidence(domain.NewEvidenceID(), "google.groups.read", now.Add(3*time.Hour))}, nil, now); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			got, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"", "google.groups.read", "google.groups.read"}, 2)
			if err != nil || len(got) != 2 || got[0].ID != second || got[1].ID != first {
				t.Fatalf("newest allowed evidence = %+v err=%v", got, err)
			}
			if got[0].InvestigationID != id || got[0].Capability != "google.groups.read" {
				t.Fatalf("lost evidence provenance: %+v", got[0])
			}
		}
		got, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"google.groups.read"}, 128)
		if err != nil || len(got) != 3 || got[2].ID != older {
			t.Fatalf("allowed history = %+v err=%v", got, err)
		}
		got[0].Data[0] = '!'
		fresh, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"google.groups.read"}, 1)
		if err != nil || !json.Valid(fresh[0].Data) {
			t.Fatalf("read evidence aliases stored JSON: %+v err=%v", fresh, err)
		}
		for _, allowed := range [][]domain.CapabilityName{nil, {""}, {"unavailable"}} {
			got, err := store.ListEvidence(ctx, id, allowed, 128)
			if err != nil || len(got) != 0 {
				t.Fatalf("unauthorized/legacy evidence disclosed: %+v err=%v", got, err)
			}
		}
		if _, err := store.ListEvidence(ctx, domain.NewInvestigationID(), nil, 128); !errors.Is(err, investigations.ErrNotFound) {
			t.Fatalf("missing case = %v", err)
		}
		events, err := store.Timeline(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var successes, failures int
		for _, event := range events {
			if event.Type != audit.EvidenceCollected && event.Type != audit.EvidenceCollectionFailed {
				continue
			}
			var data struct {
				Capability domain.CapabilityName `json:"capability"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil || data.Capability == "" {
				t.Fatalf("audit missing provenance: %s err=%v", event.Data, err)
			}
			if event.Type == audit.EvidenceCollected {
				successes++
			} else {
				failures++
				if data.Capability != "google.groups.read" {
					t.Fatalf("failure provenance = %q", data.Capability)
				}
			}
		}
		if successes != len(items) || failures != 1 {
			t.Fatalf("collection audit counts = %d/%d", successes, failures)
		}
	})

	t.Run("bounds", func(t *testing.T) {
		id := newCase(t)
		items := make([]domain.Evidence, 130)
		for i := range items {
			items[i] = evidence(domain.NewEvidenceID(), "google.groups.read", now.Add(time.Duration(i)*time.Second))
		}
		if err := store.RecordEvidence(ctx, id, items, nil, now); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct{ limit, want int }{{-1, 1}, {0, 1}, {1, 1}, {128, 128}, {129, 128}, {int(^uint(0) >> 1), 128}} {
			got, err := store.ListEvidence(ctx, id, []domain.CapabilityName{"google.groups.read"}, test.limit)
			if err != nil || len(got) != test.want || got[0].ID != items[129].ID {
				t.Fatalf("limit %d = %d rows err=%v", test.limit, len(got), err)
			}
		}
	})

	t.Run("report roundtrip and operator readmodel", func(t *testing.T) {
		id, evidenceID := newCase(t), domain.NewEvidenceID()
		if err := store.RecordEvidence(ctx, id, []domain.Evidence{evidence(evidenceID, "google.groups.read", now)}, nil, now); err != nil {
			t.Fatal(err)
		}
		before, err := store.GetInvestigation(ctx, id)
		if err != nil || before.WorkPackage != nil {
			t.Fatalf("uncompleted report = %+v err=%v", before.WorkPackage, err)
		}
		work, action, draft := report(evidenceID), evaluation(id, evidenceID), message()
		want := work.Clone()
		if err := store.CompleteInvestigation(ctx, id, "Group membership is absent", work, "local", "test-model", []investigations.ActionEvaluation{action}, draft, true, now); err != nil {
			t.Fatal(err)
		}
		work.Findings[0].EvidenceIDs[0], work.Unknowns[0] = "changed", "changed"
		item, err := store.GetInvestigation(ctx, id)
		if err != nil || item.Status != domain.InvestigationCompleted || !reflect.DeepEqual(item.WorkPackage, want) {
			t.Fatalf("report roundtrip = %+v err=%v", item, err)
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		var operator domain.Investigation
		if err := json.Unmarshal(encoded, &operator); err != nil || !reflect.DeepEqual(operator.WorkPackage, want) {
			t.Fatalf("operator detail report = %+v err=%v", operator.WorkPackage, err)
		}
		item.WorkPackage.Findings[0].Statement = "changed"
		item, err = store.GetInvestigation(ctx, id)
		if err != nil || !reflect.DeepEqual(item.WorkPackage, want) {
			t.Fatalf("returned report changed persistence: %+v err=%v", item.WorkPackage, err)
		}
		var reader admin.Reader = store
		list, err := reader.ListInvestigations(ctx, 10000)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, summary := range list {
			if summary.ID == id {
				found = true
				if summary.Status != domain.InvestigationCompleted || summary.Diagnosis != item.Diagnosis || summary.WorkPackage != nil {
					t.Fatalf("operator list should expose summary without report payload: %+v", summary)
				}
			}
		}
		if !found {
			t.Fatal("completed case missing from operator list")
		}
		messages, err := store.ListMessages(ctx, id)
		if err != nil || len(messages) != 1 || messages[0].ID != draft.ID {
			t.Fatalf("completion draft = %+v err=%v", messages, err)
		}
		var actionCount, deliveryCount int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM proposed_actions WHERE investigation_id = $1),
			(SELECT count(*) FROM message_deliveries WHERE message_id = $2)`, id, draft.ID).Scan(&actionCount, &deliveryCount); err != nil || actionCount != 1 || deliveryCount != 1 {
			t.Fatalf("completion actions/delivery = %d/%d err=%v", actionCount, deliveryCount, err)
		}
		events, err := store.Timeline(ctx, id)
		if err != nil || events[len(events)-1].Type != audit.InvestigationCompleted {
			t.Fatalf("completion audit = %+v err=%v", events, err)
		}
		if err := store.CompleteInvestigation(ctx, id, "rewrite", nil, "local", "other-model", nil, nil, false, now); !errors.Is(err, investigations.ErrInvalidTransition) {
			t.Fatalf("completed report rewrite = %v", err)
		}
		for _, sql := range []string{`UPDATE investigations SET work_package = NULL WHERE id = $1`,
			`UPDATE investigations SET work_package = jsonb_set(work_package, '{summary}', '"rewrite"') WHERE id = $1`} {
			if _, err := pool.Exec(ctx, sql, id); err == nil {
				t.Fatal("database allowed report mutation")
			}
		}
		item, err = store.GetInvestigation(ctx, id)
		if err != nil || !reflect.DeepEqual(item.WorkPackage, want) {
			t.Fatalf("immutability guard changed report: %+v err=%v", item.WorkPackage, err)
		}
	})

	t.Run("late audit failure rolls back entire completion", func(t *testing.T) {
		id, evidenceID := newCase(t), domain.NewEvidenceID()
		work, action, draft := report(evidenceID), evaluation(id, evidenceID), message()
		// Reserve the final completion event's sequence without advancing the
		// allocator. The final append fails after report/action/draft/outbox writes.
		if _, err := pool.Exec(ctx, `INSERT INTO audit_events(investigation_id, sequence, event_type, actor_type, actor_id, occurred_at, data)
			SELECT id, audit_sequence + 4, 'test_sequence_collision', 'SYSTEM', 'test', $2, '{}' FROM investigations WHERE id = $1`, id, now); err != nil {
			t.Fatal(err)
		}
		before, err := store.Timeline(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		err = store.CompleteInvestigation(ctx, id, "not committed", work, "local", "test-model", []investigations.ActionEvaluation{action}, draft, true, now)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("expected final audit unique violation, got %v", err)
		}
		item, err := store.GetInvestigation(ctx, id)
		if err != nil || item.Status != domain.InvestigationRunning || item.WorkPackage != nil || item.Diagnosis != "" || item.ModelProvider != "" || item.Model != "" || item.CompletedAt != nil {
			t.Fatalf("failed completion changed state: %+v err=%v", item, err)
		}
		var actions, messages, deliveries int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM proposed_actions WHERE investigation_id = $1),
			(SELECT count(*) FROM case_messages WHERE investigation_id = $1),
			(SELECT count(*) FROM message_deliveries WHERE message_id = $2)`, id, draft.ID).Scan(&actions, &messages, &deliveries); err != nil || actions != 0 || messages != 0 || deliveries != 0 {
			t.Fatalf("rollback actions/messages/deliveries = %d/%d/%d err=%v", actions, messages, deliveries, err)
		}
		after, err := store.Timeline(ctx, id)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("failed completion changed audit: before=%+v after=%+v err=%v", before, after, err)
		}
		var sequence int64
		if err := pool.QueryRow(ctx, `SELECT audit_sequence FROM investigations WHERE id = $1`, id).Scan(&sequence); err != nil || sequence != before[1].Sequence {
			t.Fatalf("failed completion advanced audit sequence = %d err=%v", sequence, err)
		}
	})

	t.Run("report without findings or unknowns roundtrips", func(t *testing.T) {
		id := newCase(t)
		work := &domain.WorkPackage{Version: 1, Summary: "No verified observations are available"}
		if err := store.CompleteInvestigation(ctx, id, work.Summary, work, "local", "test", nil, nil, false, now); err != nil {
			t.Fatal(err)
		}
		item, err := store.GetInvestigation(ctx, id)
		if err != nil || item.WorkPackage == nil || item.WorkPackage.Summary != work.Summary || len(item.WorkPackage.Findings) != 0 || len(item.WorkPackage.Unknowns) != 0 {
			t.Fatalf("sparse report roundtrip = %+v err=%v", item.WorkPackage, err)
		}
	})

	t.Run("report constraints and state guard", func(t *testing.T) {
		id := newCase(t)
		for _, invalid := range []string{`[]`, `null`, `{}`, `{"summary":"test"}`, `{"version":2,"summary":"test"}`,
			`{"version":1,"summary":7}`, `{"version":1,"summary":"test","findings":{}}`,
			`{"version":1,"summary":"test","findings":null}`, `{"version":1,"summary":"test","unknowns":null}`,
			`{"version":1,"summary":"test","unknowns":"unknown"}`, `{"version":1,"summary":"test","requester_summary":[]}`} {
			t.Run(fmt.Sprintf("shape_%s", invalid), func(t *testing.T) {
				_, err := pool.Exec(ctx, `UPDATE investigations SET status = 'COMPLETED', work_package = $2::jsonb WHERE id = $1`, id, invalid)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatalf("invalid report %s: expected check violation, got %v", invalid, err)
				}
			})
		}
		valid := `{"version":1,"summary":"test","findings":[],"unknowns":[]}`
		if _, err := pool.Exec(ctx, `UPDATE investigations SET work_package = $2::jsonb WHERE id = $1`, id, valid); err == nil {
			t.Fatal("database allowed report before completion")
		}
		if err := store.CompleteInvestigation(ctx, id, "legacy completion", nil, "local", "test", nil, nil, false, now); err != nil {
			t.Fatal(err)
		}
		item, err := store.GetInvestigation(ctx, id)
		if err != nil || item.WorkPackage != nil {
			t.Fatalf("nil/legacy report = %+v err=%v", item.WorkPackage, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE investigations SET work_package = $2::jsonb WHERE id = $1`, id, valid); err == nil {
			t.Fatal("database allowed attaching a report to an already completed case")
		}
	})
}
