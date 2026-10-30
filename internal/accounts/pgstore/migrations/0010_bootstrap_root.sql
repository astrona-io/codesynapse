-- The bootstrap root sets up the installation and keeps governance only
-- (docs/plans/ROOT_BOOTSTRAP_PLAN.md). Unlike platform_admin the classification is
-- set once and never removed, and there is at most one root.
ALTER TABLE users ADD COLUMN bootstrap_root boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX users_one_bootstrap_root ON users (bootstrap_root) WHERE bootstrap_root;

CREATE OR REPLACE FUNCTION users_keep_bootstrap_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.bootstrap_root AND NOT NEW.bootstrap_root THEN
        RAISE EXCEPTION 'the bootstrap root classification cannot be removed';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER users_keep_bootstrap_root BEFORE UPDATE OF bootstrap_root ON users
    FOR EACH ROW EXECUTE FUNCTION users_keep_bootstrap_root();

-- One row: whether setup handed the installation to a standard owner. An
-- installation that predates this migration is classified at API start-up
-- (accounts.Service.ReconcileBootstrap); a completed setup is never undone.
CREATE TABLE installation_setup (
    id           boolean PRIMARY KEY DEFAULT true CHECK (id),
    state        text NOT NULL CHECK (state IN ('pending', 'complete', 'ambiguous')),
    root_user_id text REFERENCES users (id),
    org_id       text REFERENCES orgs (id),
    owner_id     text REFERENCES users (id),
    completed_at timestamptz,
    CHECK (state <> 'complete' OR (root_user_id IS NOT NULL AND completed_at IS NOT NULL)),
    CHECK (state <> 'pending' OR root_user_id IS NOT NULL)
);

CREATE OR REPLACE FUNCTION installation_setup_keep_complete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state = 'complete' THEN
        RAISE EXCEPTION 'a completed setup cannot be changed';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER installation_setup_keep_complete BEFORE UPDATE OR DELETE ON installation_setup
    FOR EACH ROW EXECUTE FUNCTION installation_setup_keep_complete();
