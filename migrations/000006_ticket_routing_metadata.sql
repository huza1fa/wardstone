BEGIN;

ALTER TABLE tickets ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}'::jsonb;

INSERT INTO schema_migrations(version) VALUES (6);

COMMIT;
