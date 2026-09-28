BEGIN;

CREATE TABLE message_deliveries (
    id text PRIMARY KEY,
    message_id text NOT NULL UNIQUE REFERENCES case_messages(id),
    status text NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'SENT', 'DEAD')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    remote_id text NOT NULL DEFAULT '',
    last_error text NOT NULL DEFAULT '',
    available_at timestamptz NOT NULL,
    lease_owner text,
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL,
    delivered_at timestamptz
);
CREATE INDEX message_deliveries_claim_idx ON message_deliveries(status, available_at, lease_expires_at);

INSERT INTO schema_migrations(version) VALUES (4);

COMMIT;
