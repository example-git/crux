-- +goose Up
ALTER TABLE messages ADD COLUMN run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN item_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE messages DROP COLUMN item_id;
ALTER TABLE messages DROP COLUMN run_id;
