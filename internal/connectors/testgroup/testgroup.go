// Package testgroup is a stateful synthetic connector. It never talks to Google,
// Jira, or another infrastructure endpoint. Its state outlives service restarts.
package testgroup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/domain"
)

const AddMember domain.CapabilityName = "test.groups.add_member"

var ErrStale = errors.New("synthetic group changed after proposal; create a fresh proposal")
var ErrInvalid = errors.New("invalid synthetic operation")
var name = regexp.MustCompile(`^test-[a-z0-9][a-z0-9_-]{0,58}$`)

func ValidName(s string) bool { return name.MatchString(s) }

type Arguments struct {
	Group           string `json:"group"`
	Member          string `json:"member"`
	ExpectedVersion int64  `json:"expected_version"`
}

func Parse(raw json.RawMessage) (Arguments, error) {
	var a Arguments
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil || d.Decode(new(any)) != io.EOF || !ValidName(a.Group) || !ValidName(a.Member) || a.ExpectedVersion < 0 {
		return a, ErrInvalid
	}
	return a, nil
}
func Capability() capabilities.Definition {
	return capabilities.Definition{Name: AddMember, Connector: "test", Description: "Add a synthetic test member to a synthetic test group", Effect: domain.EffectMutate, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: func(raw json.RawMessage) error { _, err := Parse(raw); return err }}
}

type Connector struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Connector { return &Connector{pool: pool} }
func (c *Connector) Execute(ctx context.Context, capability domain.CapabilityName, raw json.RawMessage, key string) (json.RawMessage, error) {
	if capability != AddMember || key == "" {
		return nil, ErrInvalid
	}
	a, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "test-operation:"+key); err != nil {
		return nil, err
	}
	var storedArgs, storedResult json.RawMessage
	err = tx.QueryRow(ctx, `SELECT arguments,result FROM test_group_operations WHERE idempotency_key=$1`, key).Scan(&storedArgs, &storedResult)
	if err == nil {
		old, e := Parse(storedArgs)
		if e != nil || old != a {
			return nil, ErrInvalid
		}
		return storedResult, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO test_group_state(group_name) VALUES($1) ON CONFLICT DO NOTHING`, a.Group); err != nil {
		return nil, err
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT version FROM test_group_state WHERE group_name=$1 FOR UPDATE`, a.Group).Scan(&version); err != nil {
		return nil, err
	}
	if version != a.ExpectedVersion {
		return nil, ErrStale
	}
	command, err := tx.Exec(ctx, `INSERT INTO test_group_memberships(group_name,member) VALUES($1,$2) ON CONFLICT DO NOTHING`, a.Group, a.Member)
	if err != nil {
		return nil, err
	}
	changed := command.RowsAffected() == 1
	if changed {
		version++
		if _, err = tx.Exec(ctx, `UPDATE test_group_state SET version=$2 WHERE group_name=$1`, a.Group, version); err != nil {
			return nil, err
		}
	}
	result, _ := json.Marshal(map[string]any{"synthetic": true, "changed": changed, "group": a.Group, "member": a.Member, "version": version})
	if _, err = tx.Exec(ctx, `INSERT INTO test_group_operations(idempotency_key,arguments,result) VALUES($1,$2,$3)`, key, raw, result); err != nil {
		return nil, err
	}
	return result, tx.Commit(ctx)
}
func (c *Connector) Verify(ctx context.Context, action domain.ProposedAction, execution domain.Execution) (domain.Verification, error) {
	a, err := Parse(action.Arguments)
	if err != nil || action.Capability != AddMember || !action.DigestValid() {
		return domain.Verification{}, ErrInvalid
	}
	// An independent read checks both current membership and the connector's
	// receipt for this exact operation. Execution.Result is deliberately ignored.
	var member, receipt bool
	err = c.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM test_group_memberships WHERE group_name=$1 AND member=$2),EXISTS(SELECT 1 FROM test_group_operations WHERE idempotency_key=$3 AND arguments=$4::jsonb)`, a.Group, a.Member, execution.IdempotencyKey, action.Arguments).Scan(&member, &receipt)
	if err != nil {
		return domain.Verification{}, err
	}
	details, _ := json.Marshal(map[string]any{"synthetic": true, "membership_present": member, "operation_receipt_matches": receipt, "method": "independent_state_read"})
	return domain.Verification{ID: domain.NewVerificationID(), ExecutionID: execution.ID, Succeeded: member && receipt, Details: details, VerifiedAt: time.Now().UTC()}, nil
}
