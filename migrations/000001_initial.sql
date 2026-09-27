BEGIN;

CREATE TABLE schema_migrations (
    version bigint PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tickets (
    id text PRIMARY KEY,
    source text NOT NULL,
    external_id text NOT NULL,
    summary text NOT NULL,
    description text NOT NULL DEFAULT '',
    reporter_email text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    UNIQUE (source, external_id)
);

CREATE TABLE investigations (
    id text PRIMARY KEY,
    ticket_id text NOT NULL REFERENCES tickets(id),
    status text NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    model_provider text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    prompt_version text NOT NULL,
    diagnosis text NOT NULL DEFAULT '',
    failure text NOT NULL DEFAULT '',
    audit_sequence bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    completed_at timestamptz
);
CREATE INDEX investigations_ticket_id_idx ON investigations(ticket_id);

CREATE TABLE evidence (
    id text PRIMARY KEY,
    investigation_id text NOT NULL REFERENCES investigations(id),
    source text NOT NULL,
    kind text NOT NULL,
    summary text NOT NULL,
    data jsonb NOT NULL,
    observed_at timestamptz NOT NULL
);
CREATE INDEX evidence_investigation_id_idx ON evidence(investigation_id);

CREATE TABLE proposed_actions (
    id text PRIMARY KEY,
    investigation_id text NOT NULL REFERENCES investigations(id),
    capability text NOT NULL,
    arguments jsonb NOT NULL,
    reason text NOT NULL,
    evidence_ids jsonb NOT NULL,
    action_digest text NOT NULL,
    policy_decision text NOT NULL CHECK (policy_decision IN ('ALLOW', 'REQUIRE_APPROVAL', 'DENY')),
    policy_reason text NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX proposed_actions_investigation_id_idx ON proposed_actions(investigation_id);

CREATE TABLE approvals (
    id text PRIMARY KEY,
    action_id text NOT NULL REFERENCES proposed_actions(id),
    action_digest text NOT NULL,
    status text NOT NULL CHECK (status IN ('PENDING', 'GRANTED', 'DENIED', 'EXPIRED')),
    actor text NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL,
    decided_at timestamptz
);
CREATE UNIQUE INDEX approvals_one_active_per_action_idx ON approvals(action_id) WHERE status IN ('PENDING', 'GRANTED');

CREATE TABLE executions (
    id text PRIMARY KEY,
    action_id text NOT NULL REFERENCES proposed_actions(id),
    idempotency_key text NOT NULL,
    status text NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED')),
    attempt integer NOT NULL CHECK (attempt > 0),
    result jsonb,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (action_id, idempotency_key)
);

CREATE TABLE verifications (
    id text PRIMARY KEY,
    execution_id text NOT NULL REFERENCES executions(id),
    succeeded boolean NOT NULL,
    details jsonb NOT NULL,
    verified_at timestamptz NOT NULL
);

CREATE TABLE audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    investigation_id text NOT NULL REFERENCES investigations(id),
    sequence bigint NOT NULL CHECK (sequence > 0),
    event_type text NOT NULL,
    actor_type text NOT NULL,
    actor_id text NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL,
    data jsonb NOT NULL,
    UNIQUE (investigation_id, sequence)
);
CREATE INDEX audit_events_timeline_idx ON audit_events(investigation_id, sequence);

CREATE TABLE jobs (
    id text PRIMARY KEY,
    kind text NOT NULL,
    investigation_id text NOT NULL REFERENCES investigations(id),
    dedupe_key text NOT NULL UNIQUE,
    status text NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'COMPLETED', 'DEAD')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    available_at timestamptz NOT NULL,
    lease_owner text,
    lease_expires_at timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    completed_at timestamptz
);
CREATE INDEX jobs_claim_idx ON jobs(status, available_at, lease_expires_at);

CREATE FUNCTION reject_audit_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit events are append-only';
END;
$$;

CREATE TRIGGER audit_events_no_update_or_delete
BEFORE UPDATE OR DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION reject_audit_mutation();

INSERT INTO schema_migrations(version) VALUES (1);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wardstone_runtime') THEN
        CREATE ROLE wardstone_runtime LOGIN PASSWORD 'wardstone';
    END IF;
END;
$$;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO wardstone_runtime;
GRANT SELECT ON schema_migrations TO wardstone_runtime;
GRANT SELECT, INSERT, UPDATE ON tickets, investigations, evidence, proposed_actions,
    approvals, executions, verifications, jobs TO wardstone_runtime;
GRANT SELECT, INSERT ON audit_events TO wardstone_runtime;
GRANT USAGE, SELECT ON SEQUENCE audit_events_id_seq TO wardstone_runtime;

COMMIT;
