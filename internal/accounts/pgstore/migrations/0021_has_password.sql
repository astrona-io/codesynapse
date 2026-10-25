-- Reads of a user report whether there is a password without selecting the
-- hash, so a database role can be given the user columns but not password_hash.
ALTER TABLE users ADD COLUMN has_password boolean GENERATED ALWAYS AS (password_hash <> '') STORED;
