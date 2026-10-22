-- galleon:risk caution
-- galleon:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS notes_title ON notes (title);
