BEGIN;
ALTER TABLE investigations ADD COLUMN case_id text,
    ADD COLUMN parent_id text REFERENCES investigations(id);
UPDATE investigations SET case_id=id;
ALTER TABLE investigations ALTER COLUMN case_id SET NOT NULL;
-- Add the deferred self-reference after the backfill, so populated upgrades
-- have no pending foreign-key trigger events when ALTER sets NOT NULL.
ALTER TABLE investigations ADD CONSTRAINT investigations_case_id_fkey
    FOREIGN KEY(case_id) REFERENCES investigations(id) DEFERRABLE INITIALLY DEFERRED;
CREATE UNIQUE INDEX investigations_one_retry_per_attempt ON investigations(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX investigations_case_attempts ON investigations(case_id,created_at,id);
CREATE FUNCTION set_case_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.case_id IS NULL THEN NEW.case_id=NEW.id; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER investigations_set_case_root BEFORE INSERT ON investigations FOR EACH ROW EXECUTE FUNCTION set_case_root();
CREATE TABLE case_commands (
    operation text NOT NULL,
    request_id text NOT NULL,
    digest text NOT NULL,
    investigation_id text NOT NULL REFERENCES investigations(id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY(operation,request_id)
);
INSERT INTO schema_migrations(version) VALUES (11);
COMMIT;
