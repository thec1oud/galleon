-- galleon:risk safe
ALTER TABLE bookmarks ADD COLUMN created_at timestamptz;
ALTER TABLE bookmarks ALTER COLUMN created_at SET DEFAULT now();
