-- +goose Up
CREATE TABLE session_goals (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    objective TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);

-- +goose Down
DROP TABLE session_goals;
