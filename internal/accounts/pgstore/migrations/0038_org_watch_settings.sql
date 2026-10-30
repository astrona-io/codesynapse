-- Settings and state for watches that also add private repositories, and
-- name patterns a watch never adds.
--
-- include_private: list with the organisation's Git token (only while the
-- watch's creator is an owner or admin). Off for existing watches, which
-- only ever added public repositories.
-- private_since: when include_private was last turned on, or the watch
-- passed to someone else with it on.
-- private_baseline: when the watch first listed with the token after
-- private_since, or after the token was replaced. The private repositories
-- found on that scan join the baseline ("known") instead of being added, so
-- a watch never adds private repositories that existed before it could see
-- them. NULL until the first such scan.
-- exclude: lowercase name patterns (at most 20, "*" the only wildcard).
ALTER TABLE org_watches
  ADD COLUMN include_private boolean NOT NULL DEFAULT false,
  ADD COLUMN private_since timestamptz,
  ADD COLUMN private_baseline timestamptz,
  ADD COLUMN exclude jsonb NOT NULL DEFAULT '[]';
