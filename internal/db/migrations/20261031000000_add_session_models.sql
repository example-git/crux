-- +goose Up
CREATE TABLE session_model_selections (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    models TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0)
);

-- +goose Down
DROP TABLE session_model_selections;
