-- An optional second login name. Stored lower case without "@" (the service
-- validates), empty when unset; unique among the accounts that have one.
ALTER TABLE users ADD COLUMN username text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX users_username ON users (username) WHERE username <> '';
