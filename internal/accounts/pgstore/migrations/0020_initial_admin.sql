-- The administrator made at first start (accounts/initialadmin.go) is meant
-- to be replaced by the operator's own account. Mark it, and let the setup
-- record follow the root to its replacement: after a completed setup only
-- root_user_id may change, and nothing else about the record.
ALTER TABLE users ADD COLUMN auto_created boolean NOT NULL DEFAULT false;

CREATE OR REPLACE FUNCTION installation_setup_keep_complete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state = 'complete' THEN
        IF TG_OP = 'UPDATE'
           AND NEW.state = OLD.state
           AND NEW.org_id IS NOT DISTINCT FROM OLD.org_id
           AND NEW.owner_id IS NOT DISTINCT FROM OLD.owner_id
           AND NEW.completed_at IS NOT DISTINCT FROM OLD.completed_at
           AND NEW.root_user_id IS DISTINCT FROM OLD.root_user_id THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'a completed setup cannot be changed';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$$;
