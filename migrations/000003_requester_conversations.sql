BEGIN;

ALTER TABLE investigations DROP CONSTRAINT investigations_status_check;
ALTER TABLE investigations ADD CONSTRAINT investigations_status_check
    CHECK (status IN ('PENDING', 'RUNNING', 'WAITING_ON_REQUESTER', 'COMPLETED', 'FAILED', 'CANCELLED'));

CREATE TABLE case_messages (
    id text PRIMARY KEY,
    investigation_id text NOT NULL REFERENCES investigations(id),
    source text NOT NULL,
    external_id text NOT NULL,
    direction text NOT NULL CHECK (direction IN ('OUTBOUND', 'INBOUND')),
    author text NOT NULL DEFAULT '',
    body text NOT NULL CHECK (length(body) > 0 AND length(body) <= 16000),
    created_at timestamptz NOT NULL,
    UNIQUE (source, external_id)
);
CREATE INDEX case_messages_investigation_idx ON case_messages(investigation_id, created_at, id);

CREATE TRIGGER case_messages_no_update_or_delete
BEFORE UPDATE OR DELETE ON case_messages
FOR EACH ROW EXECUTE FUNCTION reject_audit_mutation();

GRANT SELECT, INSERT ON case_messages TO wardstone_runtime;

INSERT INTO schema_migrations(version) VALUES (3);

COMMIT;
