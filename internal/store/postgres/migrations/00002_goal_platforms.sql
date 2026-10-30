-- +goose Up
ALTER TABLE goals ADD COLUMN platforms text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE goals DROP COLUMN platforms;
