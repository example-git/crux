-- +goose Up
CREATE TABLE session_instructions (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('inherit', 'append', 'replace')),
    text TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0)
);

-- +goose Down
DROP TABLE session_instructions;
