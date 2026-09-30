BEGIN;

-- Legacy cases retain NULL: their original routing provenance is unknown.
ALTER TABLE investigations ADD COLUMN routing jsonb;

ALTER TABLE investigations ADD CONSTRAINT investigations_routing_object
    CHECK (routing IS NULL OR jsonb_typeof(routing) = 'object');

-- Handoffs can update the active specialist, but never rewrite the original
-- routing decision. The dispatch transaction appends its audit event atomically.
CREATE FUNCTION preserve_investigation_routing() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.routing IS NOT NULL AND NEW.routing IS DISTINCT FROM OLD.routing THEN
        RAISE EXCEPTION 'investigation routing decision is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER investigations_preserve_routing
    BEFORE UPDATE OF routing ON investigations
    FOR EACH ROW EXECUTE FUNCTION preserve_investigation_routing();

INSERT INTO schema_migrations(version) VALUES (7);

COMMIT;
