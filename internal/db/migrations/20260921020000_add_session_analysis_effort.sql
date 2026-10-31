-- +goose Up
ALTER TABLE sessions ADD COLUMN analysis_effort TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN analysis_effort;
