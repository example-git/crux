-- +goose Up
CREATE TABLE path_translation_scopes (
    scope_key TEXT PRIMARY KEY NOT NULL,
    settings TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE path_translation_identity (
    id INTEGER PRIMARY KEY NOT NULL CHECK (id = 1),
    secret BLOB NOT NULL CHECK (length(secret) = 32)
);

CREATE TABLE path_translation_names (
    identity TEXT PRIMARY KEY NOT NULL,
    alias TEXT UNIQUE NOT NULL,
    path TEXT UNIQUE NOT NULL
);

-- +goose Down
DROP TABLE path_translation_names;
DROP TABLE path_translation_identity;
DROP TABLE path_translation_scopes;
