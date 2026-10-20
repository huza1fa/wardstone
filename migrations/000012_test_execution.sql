BEGIN;
ALTER TABLE executions DROP CONSTRAINT executions_status_check;
ALTER TABLE executions ADD CONSTRAINT executions_status_check CHECK(status IN('PENDING','RUNNING','VERIFYING','SUCCEEDED','FAILED','NEEDS_RECONCILIATION'));
ALTER TABLE executions ADD COLUMN approval_id text REFERENCES approvals(id),
    ADD COLUMN lease_owner text, ADD COLUMN lease_expires_at timestamptz;
CREATE UNIQUE INDEX executions_one_test_action ON executions(action_id) WHERE approval_id IS NOT NULL;
CREATE INDEX executions_claim_idx ON executions(status,lease_expires_at);
CREATE TABLE test_group_state (
    group_name text PRIMARY KEY CHECK(group_name ~ '^test-[a-z0-9][a-z0-9_-]{0,58}$'),
    version bigint NOT NULL DEFAULT 0 CHECK(version>=0)
);
CREATE TABLE test_group_memberships (
    group_name text NOT NULL REFERENCES test_group_state(group_name),
    member text NOT NULL CHECK(member ~ '^test-[a-z0-9][a-z0-9_-]{0,58}$'),
    PRIMARY KEY(group_name,member)
);
-- This is connector state, committed independently of orchestration records.
-- A durable receipt lets a recovered execution reuse its exact operation key.
CREATE TABLE test_group_operations (
    idempotency_key text PRIMARY KEY,
    arguments jsonb NOT NULL,
    result jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations(version) VALUES (12);
COMMIT;
