-- A person's own choices for the repository menu: the pages they pinned to
-- the top and the ones they hid. Slugs only, validated by the service.
CREATE TABLE menu_prefs (
    user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    pinned  text[] NOT NULL DEFAULT '{}',
    hidden  text[] NOT NULL DEFAULT '{}'
);
