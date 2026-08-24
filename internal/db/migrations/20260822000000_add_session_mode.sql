-- +goose Up
ALTER TABLE sessions ADD COLUMN mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE sessions ADD COLUMN plan TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN plan;
ALTER TABLE sessions DROP COLUMN mode;
