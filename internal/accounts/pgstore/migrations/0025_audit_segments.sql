-- Sealed runs of the oldest audit entries. The entries move to an archive
-- file (a directory or S3-compatible object storage) and leave the events
-- table; the hashes stay here, so the chain verifies across segments without
-- the files. After retention the oldest row goes with its file, and the next
-- segment's prev_hash becomes the anchor.
CREATE TABLE audit_segments (
    id         bigserial PRIMARY KEY,
    first_id   bigint NOT NULL UNIQUE,
    last_id    bigint NOT NULL,
    first_at   timestamptz NOT NULL,
    last_at    timestamptz NOT NULL,
    count      int NOT NULL CHECK (count > 0),
    prev_hash  text NOT NULL,
    last_hash  text NOT NULL,
    sha256     text NOT NULL,
    bytes      bigint NOT NULL,
    backend    text NOT NULL,
    key        text NOT NULL,
    sealed_at  timestamptz NOT NULL,
    CHECK (first_id <= last_id)
);
