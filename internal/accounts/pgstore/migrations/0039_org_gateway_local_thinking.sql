-- The gateway offers requests that ask for thinking to the local model
-- first; the cloud answers when the local answer fails. It is on unless an
-- owner or admin turns it off, also for organisations that already have a row.
ALTER TABLE org_gateway ADD COLUMN local_thinking boolean NOT NULL DEFAULT true;
