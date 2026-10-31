-- +goose Up
ALTER TABLE messages ADD COLUMN submission_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE messages DROP COLUMN submission_id;
