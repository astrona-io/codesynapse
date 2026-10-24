-- The browser that signed in, so the profile page can tell sessions apart.
ALTER TABLE sessions ADD COLUMN user_agent text NOT NULL DEFAULT '';
