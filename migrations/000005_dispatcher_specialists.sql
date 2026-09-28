BEGIN;

ALTER TABLE investigations
    ADD COLUMN specialist text NOT NULL DEFAULT '';
CREATE INDEX investigations_specialist_status_idx
    ON investigations(specialist, status)
    WHERE specialist <> '';

INSERT INTO schema_migrations(version) VALUES (5);

COMMIT;
