package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wardstone-project/wardstone/internal/api"
	"github.com/wardstone-project/wardstone/internal/approvals"
	"github.com/wardstone-project/wardstone/internal/connectors/testgroup"
	"github.com/wardstone-project/wardstone/internal/database"
	"github.com/wardstone-project/wardstone/internal/domain"
	"github.com/wardstone-project/wardstone/internal/executor"
)

type testExecutionFixture struct {
	pool      *pgxpool.Pool
	store     *database.Store
	connector *testgroup.Connector
	pilot     *executor.Pilot
	server    *api.Server
}

func newTestExecutionFixture(t *testing.T, enabled bool) testExecutionFixture {
	t.Helper()
	pool := routingTestDatabase(t)
	store := database.NewStore(pool)
	connector := testgroup.New(pool)
	pilot, err := executor.NewPilot(store, connector, connector, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := approvals.NewService(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.NewServer(nil, store, "", "pilot-operator", api.WithAdmin(store, approval, domain.OperatingModeShadow), api.WithTestExecution(store, enabled, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return testExecutionFixture{pool, store, connector, pilot, server}
}
func (f testExecutionFixture) propose(t *testing.T, key, group, member string) executor.Summary {
	t.Helper()
	var result struct {
		Item executor.Summary `json:"item"`
	}
	pilotHTTP(t, f.server, "POST", "/v1/admin/test-executions", "pilot-operator", executor.Proposal{Command: caseCommand(key), Group: group, Member: member}, 202, &result)
	return result.Item
}
func (f testExecutionFixture) approveAndQueue(t *testing.T, item executor.Summary, key string) domain.ExecutionID {
	t.Helper()
	pilotHTTP(t, f.server, "POST", "/v1/admin/approvals/"+string(item.Approval.ID)+"/decision", "pilot-operator", map[string]string{"decision": "GRANTED", "actor": "test-admin"}, 200, nil)
	var result struct {
		ID domain.ExecutionID `json:"execution_id"`
	}
	path := "/v1/admin/test-executions/approvals/" + string(item.Approval.ID) + "/queue"
	pilotHTTP(t, f.server, "POST", path, "pilot-operator", caseCommand(key), 202, &result)
	var again struct {
		ID domain.ExecutionID `json:"execution_id"`
	}
	pilotHTTP(t, f.server, "POST", path, "pilot-operator", caseCommand(key), 202, &again)
	if result.ID != again.ID {
		t.Fatal("queue replay created a second execution")
	}
	return result.ID
}
func (f testExecutionFixture) claim(t *testing.T) executor.Job {
	t.Helper()
	job, err := f.store.ClaimExecution(context.Background(), string(domain.NewJobID()), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func (f testExecutionFixture) expire(t *testing.T, job executor.Job) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE executions SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, job.Execution.ID); err != nil {
		t.Fatal(err)
	}
}
func (f testExecutionFixture) assertCounts(t *testing.T, operations, members, verifications int) {
	t.Helper()
	for _, tc := range []struct {
		table string
		want  int
	}{{"test_group_operations", operations}, {"test_group_memberships", members}, {"verifications", verifications}} {
		var n int
		if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+tc.table).Scan(&n); err != nil || n != tc.want {
			t.Fatalf("%s count=%d want=%d err=%v", tc.table, n, tc.want, err)
		}
	}
}
func TestApprovedTestExecutionCrashRecovery(t *testing.T) {
	for _, phase := range []string{"normal", "before mutation", "after mutation before acknowledgement", "after acknowledgement before verification"} {
		t.Run(phase, func(t *testing.T) {
			f := newTestExecutionFixture(t, true)
			item := f.propose(t, "propose-crash-test", "test-team", "test-alice")
			path := "/v1/admin/test-executions/approvals/" + string(item.Approval.ID) + "/queue"
			pilotHTTP(t, f.server, "POST", path, "pilot-operator", caseCommand("queue-without-grant"), 409, nil)
			f.approveAndQueue(t, item, "queue-crash-test")
			job := f.claim(t)
			if _, err := f.store.ClaimExecution(context.Background(), "competitor", time.Minute); !errors.Is(err, executor.ErrNoExecution) {
				t.Fatalf("duplicate claim: %v", err)
			}
			if phase != "normal" {
				if phase != "before mutation" {
					raw, err := f.connector.Execute(context.Background(), job.Action.Capability, job.Action.Arguments, job.Execution.IdempotencyKey)
					if err != nil {
						t.Fatal(err)
					}
					if phase == "after acknowledgement before verification" {
						if err := f.store.RecordMutation(context.Background(), job, raw, time.Now()); err != nil {
							t.Fatal(err)
						}
					}
				}
				f.expire(t, job)
				previous := job
				job = f.claim(t)
				if err := f.store.RecordMutation(context.Background(), previous, []byte(`{}`), time.Now()); !errors.Is(err, executor.ErrLeaseLost) {
					t.Fatalf("stale worker wrote: %v", err)
				}
			}
			// Recreate all in-process components to prove durable recovery.
			connector := testgroup.New(f.pool)
			mutator := &countingTestMutator{connector: connector}
			pilot, err := executor.NewPilot(database.NewStore(f.pool), mutator, connector, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := pilot.RunJob(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			if phase == "after acknowledgement before verification" && mutator.calls != 0 {
				t.Fatal("VERIFYING recovery repeated mutation")
			}
			f.assertCounts(t, 1, 1, 1)
			items, err := f.store.ListTestExecutions(context.Background())
			if err != nil || len(items) != 1 || items[0].Execution.Status != "SUCCEEDED" || !items[0].Verification.Succeeded {
				t.Fatalf("execution=%+v %v", items, err)
			}
			if _, err := f.store.ClaimExecution(context.Background(), "after-success", time.Minute); !errors.Is(err, executor.ErrNoExecution) {
				t.Fatal("completed execution reclaimed")
			}
			var jobs, deliveries int
			_ = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM jobs),(SELECT count(*) FROM message_deliveries)`).Scan(&jobs, &deliveries)
			if jobs != 0 || deliveries != 0 {
				t.Fatal("synthetic pilot enqueued real workflow jobs")
			}
		})
	}
}

type countingTestMutator struct {
	connector *testgroup.Connector
	calls     int
}

func (c *countingTestMutator) Execute(ctx context.Context, capability domain.CapabilityName, args json.RawMessage, key string) (json.RawMessage, error) {
	c.calls++
	return c.connector.Execute(ctx, capability, args, key)
}

func TestSyntheticExecutionPreconditionsAndReadOnlyRecovery(t *testing.T) {
	f := newTestExecutionFixture(t, true)
	a := f.propose(t, "propose-precondition-a", "test-shared", "test-alice")
	b := f.propose(t, "propose-precondition-b", "test-shared", "test-bob")
	f.approveAndQueue(t, a, "queue-precondition-a")
	f.approveAndQueue(t, b, "queue-precondition-b")
	first := f.claim(t)
	if err := f.pilot.RunJob(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := f.claim(t)
	if err := f.pilot.RunJob(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	items, err := f.store.ListTestExecutions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, item := range items {
		if item.Execution.Status == "FAILED" {
			failed++
			if item.Execution.Error != "test_precondition_changed" {
				t.Fatal(item.Execution.Error)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("stale precondition accepted: %+v", items)
	}
	f.assertCounts(t, 1, 1, 1)
	// Drift after a committed mutation must fail verification, regardless of a
	// successful mutation response. Explicit rechecks perform no mutation.
	c := f.propose(t, "propose-drift-test", "test-drift", "test-charlie")
	executionID := f.approveAndQueue(t, c, "queue-drift-test")
	job := f.claim(t)
	raw, err := f.connector.Execute(context.Background(), job.Action.Capability, job.Action.Arguments, job.Execution.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordMutation(context.Background(), job, raw, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM test_group_memberships WHERE group_name='test-drift'`); err != nil {
		t.Fatal(err)
	}
	f.expire(t, job)
	verifyJob := f.claim(t)
	mutator := &countingTestMutator{connector: f.connector}
	pilot, err := executor.NewPilot(f.store, mutator, f.connector, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := pilot.RunJob(context.Background(), verifyJob); err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/test-executions/" + string(executionID) + "/verify"
	pilotHTTP(t, f.server, "POST", path, "pilot-operator", caseCommand("verify-drift-once"), 202, nil)
	pilotHTTP(t, f.server, "POST", path, "pilot-operator", caseCommand("verify-drift-once"), 202, nil)
	verifyJob = f.claim(t)
	if err := pilot.RunJob(context.Background(), verifyJob); err != nil {
		t.Fatal(err)
	}
	if mutator.calls != 0 {
		t.Fatal("explicit verification mutated state")
	}
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM executions WHERE id=$1`, executionID).Scan(&status); err != nil || status != "FAILED" {
		t.Fatalf("drift verification status=%s err=%v", status, err)
	}
	// An operator fixes synthetic connector state independently, then rechecks.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO test_group_memberships(group_name,member) VALUES('test-drift','test-charlie')`); err != nil {
		t.Fatal(err)
	}
	pilotHTTP(t, f.server, "POST", path, "pilot-operator", caseCommand("verify-drift-fixed"), 202, nil)
	if err := pilot.RunJob(context.Background(), f.claim(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM executions WHERE id=$1`, executionID).Scan(&status); err != nil || status != "SUCCEEDED" || mutator.calls != 0 {
		t.Fatalf("recovered status=%s err=%v calls=%d", status, err, mutator.calls)
	}
}

func TestSyntheticPilotDisabledExpiryAndBoundedRecovery(t *testing.T) {
	disabled := newTestExecutionFixture(t, false)
	payload := executor.Proposal{Command: caseCommand("disabled-proposal"), Group: "test-team", Member: "test-alice"}
	pilotHTTP(t, disabled.server, "POST", "/v1/admin/test-executions", "pilot-operator", payload, 503, nil)
	pilotHTTP(t, disabled.server, "POST", "/v1/admin/test-executions", "unauthorized", payload, 401, nil)
	disabled.assertCounts(t, 0, 0, 0)
	f := newTestExecutionFixture(t, true)
	payload.Group = "engineering@example.com"
	pilotHTTP(t, f.server, "POST", "/v1/admin/test-executions", "pilot-operator", payload, 400, nil)
	item := f.propose(t, "propose-expired-test", "test-expiry", "test-alice")
	f.approveAndQueue(t, item, "queue-expired-test")
	if _, err := f.pool.Exec(context.Background(), `UPDATE approvals SET status='EXPIRED' WHERE id=$1`, item.Approval.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimExecution(context.Background(), "late-worker", time.Minute); !errors.Is(err, executor.ErrNoExecution) {
		t.Fatalf("expired grant claimed: %v", err)
	}
	f.assertCounts(t, 0, 0, 0)
	var status string
	_ = f.pool.QueryRow(context.Background(), `SELECT status FROM executions WHERE action_id=$1`, item.Action.ID).Scan(&status)
	if status != "NEEDS_RECONCILIATION" {
		t.Fatal(status)
	}
	next := f.propose(t, "propose-recovery-bound", "test-recovery", "test-bob")
	f.approveAndQueue(t, next, "queue-recovery-bound")
	for range 3 {
		job := f.claim(t)
		f.expire(t, job)
	}
	if _, err := f.store.ClaimExecution(context.Background(), "exhausted-worker", time.Minute); !errors.Is(err, executor.ErrNoExecution) {
		t.Fatalf("unbounded recovery: %v", err)
	}
	_ = f.pool.QueryRow(context.Background(), `SELECT status FROM executions WHERE action_id=$1`, next.Action.ID).Scan(&status)
	if status != "NEEDS_RECONCILIATION" {
		t.Fatal(status)
	}
	f.assertCounts(t, 0, 0, 0)
}
