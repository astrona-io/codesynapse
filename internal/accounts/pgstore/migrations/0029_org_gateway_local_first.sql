-- The gateway offers every model's requests to the local model first unless
-- an organisation turns that off (its local model may be too small for the
-- main model). Rows already stored and rows written only for "enabled" stay
-- local first.
ALTER TABLE org_gateway ADD COLUMN local_first boolean NOT NULL DEFAULT true;
