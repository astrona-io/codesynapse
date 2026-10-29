-- The gateway may have the local model shorten prose a person typed before
-- a request goes to the cloud. That alters the person's words, so it is off
-- until an organisation turns it on; rows already stored stay off.
ALTER TABLE org_gateway ADD COLUMN shorten_prompts boolean NOT NULL DEFAULT false;
