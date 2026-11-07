-- +goose Up
-- The agent CLIs each worker reports, with where each came from and the code
-- of a failed install (docs/worker.md, Agent install).
ALTER TABLE workers ADD COLUMN agents jsonb NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE workers DROP COLUMN agents;
