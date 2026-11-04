-- +goose Up
-- Knowledge embeddings are 1536 numbers, the size of OpenAI's
-- text-embedding-3-small (knowledge.md, Search). Only where 00004 found
-- pgvector: the column gets its dimension and a cosine index.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema() AND table_name = 'knowledge_entries' AND column_name = 'embedding') THEN
    ALTER TABLE knowledge_entries ALTER COLUMN embedding TYPE vector(1536);
    CREATE INDEX IF NOT EXISTS knowledge_entries_embedding ON knowledge_entries USING hnsw (embedding vector_cosine_ops);
  END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema() AND table_name = 'knowledge_entries' AND column_name = 'embedding') THEN
    DROP INDEX IF EXISTS knowledge_entries_embedding;
    ALTER TABLE knowledge_entries ALTER COLUMN embedding TYPE vector;
  END IF;
END $$;
-- +goose StatementEnd
