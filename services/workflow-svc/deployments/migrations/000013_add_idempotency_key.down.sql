-- 000013_add_idempotency_key.down.sql
-- Rollback idempotency_key column

DROP INDEX IF EXISTS idx_workflow_instances_idempotency_key;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS idempotency_key;