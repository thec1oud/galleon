-- galleon:risk safe
CREATE TABLE bookmarks (
    id    bigserial PRIMARY KEY,
    url   text NOT NULL,
    title text NOT NULL
);
