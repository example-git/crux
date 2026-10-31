-- +goose Up
ALTER TABLE sessions ADD COLUMN forked_from_session_id TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN forked_from_session_id;
