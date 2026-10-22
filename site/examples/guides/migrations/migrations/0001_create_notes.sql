-- galleon:risk safe
CREATE TABLE notes (
    id    bigserial PRIMARY KEY,
    title text NOT NULL,
    body  text NOT NULL DEFAULT ''
);
