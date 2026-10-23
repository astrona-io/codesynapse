-- Login attempts shared by every API replica (docs/SAAS.md, 6c). Keys are
-- SHA-256 hashes of the email or client address, never the values.
CREATE TABLE login_attempts (
    key text NOT NULL,
    at  timestamptz NOT NULL
);
CREATE INDEX login_attempts_key ON login_attempts (key);
CREATE INDEX login_attempts_at ON login_attempts (at);
