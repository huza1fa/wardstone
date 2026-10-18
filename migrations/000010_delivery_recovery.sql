BEGIN;

ALTER TABLE message_deliveries DROP CONSTRAINT message_deliveries_status_check;
-- Old errors were untyped. Preserve their text and conservatively require
-- reconciliation unless the row is known to have been superseded before send.
UPDATE message_deliveries SET status = CASE WHEN last_error='draft_superseded'
    THEN 'SUPERSEDED' ELSE 'NEEDS_RECONCILIATION' END WHERE status='DEAD';
ALTER TABLE message_deliveries ADD CONSTRAINT message_deliveries_status_check
    CHECK (status IN ('PENDING','RUNNING','SENT','FAILED','NEEDS_RECONCILIATION','SUPERSEDED'));
ALTER TABLE message_deliveries ADD COLUMN revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0);

ALTER TABLE message_deliveries ADD COLUMN correlation_required boolean NOT NULL DEFAULT false;

INSERT INTO schema_migrations(version) VALUES (10);
COMMIT;
