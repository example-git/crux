-- +goose Up
ALTER TABLE sessions ADD COLUMN estimated_usage INTEGER NOT NULL DEFAULT 0 CHECK (estimated_usage IN (0, 1));

-- +goose Down
ALTER TABLE sessions DROP COLUMN estimated_usage;
