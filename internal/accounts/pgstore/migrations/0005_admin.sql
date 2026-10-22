-- Suspended users cannot sign in and their sessions and API keys stop
-- working (offboarding, incidents); the account and its history remain.
ALTER TABLE users ADD COLUMN disabled_at timestamptz;
CREATE INDEX events_action ON events (action);
