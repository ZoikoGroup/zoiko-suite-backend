ALTER TABLE search_evidence DROP COLUMN IF EXISTS embedding_model;
ALTER TABLE index_generations DROP CONSTRAINT IF EXISTS index_generations_backfill_state;
ALTER TABLE index_generations DROP COLUMN IF EXISTS backfill_note;
ALTER TABLE index_generations DROP COLUMN IF EXISTS backfill_state;
