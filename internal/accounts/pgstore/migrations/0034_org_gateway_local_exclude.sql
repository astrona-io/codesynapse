-- Model names, or prefixes ending in "*", whose requests the gateway never
-- offers to the local model, whatever local_first says. Validated,
-- lower-cased, deduplicated and sorted by the service (internal/modelpattern);
-- the check keeps a bad write from growing the row. Empty unless an owner or
-- admin sets it.
ALTER TABLE org_gateway ADD COLUMN local_exclude text[] NOT NULL DEFAULT '{}'
    CHECK (cardinality(local_exclude) <= 32);
