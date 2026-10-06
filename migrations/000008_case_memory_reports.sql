BEGIN;

-- Historical evidence has unknown provenance. Never infer it from source/kind.
ALTER TABLE evidence ADD COLUMN capability text NOT NULL DEFAULT '';
CREATE INDEX evidence_case_capability_newest_idx
    ON evidence(investigation_id, capability, observed_at DESC, id DESC)
    WHERE capability <> '';

ALTER TABLE investigations ADD COLUMN work_package jsonb;
ALTER TABLE investigations ADD CONSTRAINT investigations_work_package_shape
    CHECK (work_package IS NULL OR COALESCE(
        jsonb_typeof(work_package) = 'object'
        AND work_package->'version' = '1'::jsonb
        AND jsonb_typeof(work_package->'summary') = 'string'
        AND (NOT work_package ? 'findings' OR jsonb_typeof(work_package->'findings') = 'array')
        AND (NOT work_package ? 'unknowns' OR jsonb_typeof(work_package->'unknowns') = 'array')
        AND (NOT work_package ? 'escalation_reason' OR jsonb_typeof(work_package->'escalation_reason') = 'string')
        AND (NOT work_package ? 'requester_summary' OR jsonb_typeof(work_package->'requester_summary') = 'string')
        AND (NOT work_package ? 'expected_impact' OR jsonb_typeof(work_package->'expected_impact') = 'string'),
        false));
ALTER TABLE investigations ADD CONSTRAINT investigations_work_package_completed
    CHECK (work_package IS NULL OR status = 'COMPLETED');

-- A report is published exactly once by the RUNNING -> COMPLETED transition.
CREATE FUNCTION preserve_investigation_work_package() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.work_package IS DISTINCT FROM OLD.work_package THEN
        IF OLD.work_package IS NOT NULL THEN
            RAISE EXCEPTION 'investigation work package is immutable';
        END IF;
        IF OLD.status <> 'RUNNING' OR NEW.status <> 'COMPLETED' THEN
            RAISE EXCEPTION 'investigation work package requires completion transition';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER investigations_preserve_work_package
    BEFORE UPDATE OF work_package ON investigations
    FOR EACH ROW EXECUTE FUNCTION preserve_investigation_work_package();

INSERT INTO schema_migrations(version) VALUES (8);

COMMIT;
