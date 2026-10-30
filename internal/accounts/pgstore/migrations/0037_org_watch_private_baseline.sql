-- When a watch first listed with its organisation's Git token, or first did
-- so after that token was replaced. The private repositories found on that
-- scan join the baseline ("known") instead of being added, so a watch never
-- adds private repositories that existed before it could see them. NULL
-- until the first such scan.
ALTER TABLE org_watches ADD COLUMN private_baseline timestamptz;
