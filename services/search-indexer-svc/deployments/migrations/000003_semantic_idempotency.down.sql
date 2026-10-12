DROP INDEX IF EXISTS idx_restriction_tombstones_failed;

DROP POLICY IF EXISTS tenant_isolation_policy ON idempotency_keys;
DROP TABLE IF EXISTS idempotency_keys;

DROP TABLE IF EXISTS retrieval_evaluations;

ALTER TABLE index_contracts DROP CONSTRAINT IF EXISTS index_contracts_embedding_complete;
ALTER TABLE index_contracts DROP COLUMN IF EXISTS embedding_similarity;
ALTER TABLE index_contracts DROP COLUMN IF EXISTS embedding_preprocessing;
ALTER TABLE index_contracts DROP COLUMN IF EXISTS embedding_source_fields;
ALTER TABLE index_contracts DROP COLUMN IF EXISTS embedding_dimensions;
ALTER TABLE index_contracts DROP COLUMN IF EXISTS embedding_model_version;
ALTER TABLE index_contracts DROP COLUMN IF EXISTS embedding_model;
