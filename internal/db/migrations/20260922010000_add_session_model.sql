-- +goose Up
ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN provider TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN provider;
ALTER TABLE sessions DROP COLUMN model;
