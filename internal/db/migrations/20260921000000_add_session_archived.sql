-- +goose Up
ALTER TABLE sessions ADD COLUMN archived INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1));

-- +goose Down
ALTER TABLE sessions DROP COLUMN archived;
