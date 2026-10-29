-- The gateway has the local model shorten prose a person typed before a
-- request goes to the cloud. It is on unless an owner or admin turns it off,
-- also for organisations that already have a row.
ALTER TABLE org_gateway ADD COLUMN shorten_prompts boolean NOT NULL DEFAULT true;
