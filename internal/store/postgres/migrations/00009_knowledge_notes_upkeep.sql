-- +goose Up
-- Upkeep of agent notes (knowledge.md): how many searches returned an entry,
-- and when a note was promoted into a proposal for a curated section. A
-- promoted note never expires.
ALTER TABLE knowledge_entries
  ADD COLUMN hits        integer NOT NULL DEFAULT 0 CHECK (hits >= 0),
  ADD COLUMN promoted_at timestamptz,
  ADD CONSTRAINT knowledge_entries_promoted_note CHECK (promoted_at IS NULL OR provider = 'internal');

-- +goose Down
ALTER TABLE knowledge_entries
  DROP CONSTRAINT knowledge_entries_promoted_note,
  DROP COLUMN promoted_at,
  DROP COLUMN hits;
