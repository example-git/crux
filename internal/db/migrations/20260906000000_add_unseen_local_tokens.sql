-- +goose Up
ALTER TABLE sessions ADD COLUMN unseen_local_tokens INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN unseen_local_tokens;
