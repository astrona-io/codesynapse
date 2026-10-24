-- When each user last marked the dashboard's notifications read. The
-- notifications themselves are derived from snapshots, jobs and reviews
-- when they are read, so only this mark is stored.
CREATE TABLE notifications_seen (
    user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    seen_at timestamptz NOT NULL
);
