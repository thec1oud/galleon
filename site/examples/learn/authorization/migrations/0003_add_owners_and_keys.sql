-- galleon:risk safe
ALTER TABLE bookmarks ADD COLUMN owner text;

CREATE TABLE api_keys (
    digest     bytea PRIMARY KEY,
    owner      text NOT NULL,
    roles      text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
