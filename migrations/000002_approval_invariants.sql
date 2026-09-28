BEGIN;

CREATE FUNCTION reject_proposed_action_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'proposed actions are immutable';
END;
$$;

CREATE TRIGGER proposed_actions_no_update_or_delete
BEFORE UPDATE OR DELETE ON proposed_actions
FOR EACH ROW EXECUTE FUNCTION reject_proposed_action_mutation();

CREATE FUNCTION enforce_approval_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    persisted_digest text;
    persisted_policy text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT action_digest, policy_decision INTO persisted_digest, persisted_policy
        FROM proposed_actions WHERE id = NEW.action_id;

        IF NOT FOUND OR NEW.action_digest <> persisted_digest OR persisted_policy <> 'REQUIRE_APPROVAL' THEN
            RAISE EXCEPTION 'approval must bind to an action that requires approval';
        END IF;
        IF NEW.status <> 'PENDING' OR NEW.actor <> '' OR NEW.decided_at IS NOT NULL THEN
            RAISE EXCEPTION 'new approvals must be pending and undecided';
        END IF;
        IF NEW.expires_at <= CURRENT_TIMESTAMP OR NEW.expires_at > CURRENT_TIMESTAMP + INTERVAL '7 days' THEN
            RAISE EXCEPTION 'approval expiry must be within 7 days';
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'approvals cannot be deleted';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.action_id IS DISTINCT FROM OLD.action_id
        OR NEW.action_digest IS DISTINCT FROM OLD.action_digest
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'approval binding and expiry are immutable';
    END IF;

    IF NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('GRANTED', 'DENIED', 'EXPIRED'))
        OR (OLD.status = 'GRANTED' AND NEW.status = 'EXPIRED')
    ) THEN
        RAISE EXCEPTION 'invalid approval transition from % to %', OLD.status, NEW.status;
    END IF;

    IF NEW.status IN ('GRANTED', 'DENIED')
        AND (NEW.actor = '' OR NEW.decided_at IS NULL) THEN
        RAISE EXCEPTION 'approval decisions require an actor and decision time';
    END IF;

    IF NEW.status IN ('GRANTED', 'DENIED') AND OLD.expires_at <= CURRENT_TIMESTAMP THEN
        RAISE EXCEPTION 'expired approvals cannot be decided';
    END IF;

    IF NEW.status = 'EXPIRED'
        AND (NEW.actor IS DISTINCT FROM OLD.actor OR NEW.decided_at IS DISTINCT FROM OLD.decided_at) THEN
        RAISE EXCEPTION 'expiration cannot alter prior decision metadata';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER approvals_enforce_transition
BEFORE INSERT OR UPDATE OR DELETE ON approvals
FOR EACH ROW EXECUTE FUNCTION enforce_approval_transition();

ALTER TABLE approvals
    ADD CONSTRAINT approvals_actor_length CHECK (char_length(actor) <= 256),
    ADD CONSTRAINT approvals_decision_metadata CHECK (
        (status = 'PENDING' AND actor = '' AND decided_at IS NULL)
        OR (status IN ('GRANTED', 'DENIED') AND actor <> '' AND decided_at IS NOT NULL)
        OR (status = 'EXPIRED' AND (
            (actor = '' AND decided_at IS NULL)
            OR (actor <> '' AND decided_at IS NOT NULL)
        ))
    );

INSERT INTO schema_migrations(version) VALUES (2);

COMMIT;
