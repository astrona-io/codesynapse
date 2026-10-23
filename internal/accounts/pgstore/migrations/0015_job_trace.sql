-- The W3C traceparent of the request that queued a job, so the worker's
-- spans continue the request's trace (docs/SAAS.md, 5e).
ALTER TABLE jobs ADD COLUMN trace text NOT NULL DEFAULT '';
