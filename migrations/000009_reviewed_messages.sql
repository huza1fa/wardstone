BEGIN;

ALTER TABLE message_deliveries ADD COLUMN reviewed_body text;
ALTER TABLE message_deliveries ADD COLUMN reviewed_by text;
ALTER TABLE message_deliveries ADD COLUMN reviewed_at timestamptz;
ALTER TABLE message_deliveries ADD COLUMN draft_digest text;
ALTER TABLE message_deliveries ADD CONSTRAINT message_review_shape CHECK (
    (reviewed_body IS NULL AND reviewed_by IS NULL AND reviewed_at IS NULL AND draft_digest IS NULL)
    OR (reviewed_body IS NOT NULL AND octet_length(reviewed_body) BETWEEN 1 AND 16000
        AND reviewed_by IS NOT NULL AND octet_length(reviewed_by) BETWEEN 1 AND 256
        AND reviewed_at IS NOT NULL AND draft_digest IS NOT NULL
        AND draft_digest ~ '^[0-9a-f]{64}$'));

CREATE FUNCTION preserve_message_review() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.reviewed_body IS DISTINCT FROM OLD.reviewed_body
        OR NEW.reviewed_by IS DISTINCT FROM OLD.reviewed_by
        OR NEW.reviewed_at IS DISTINCT FROM OLD.reviewed_at
        OR NEW.draft_digest IS DISTINCT FROM OLD.draft_digest THEN
        RAISE EXCEPTION 'requester message review is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER message_deliveries_preserve_review BEFORE UPDATE ON message_deliveries
    FOR EACH ROW EXECUTE FUNCTION preserve_message_review();

INSERT INTO schema_migrations(version) VALUES (9);
COMMIT;
