-- 000007_workflow_mapping.down.sql
-- Obligations Service — obligation-to-workflow mappings rollback

BEGIN;

DROP INDEX IF EXISTS idx_obligations_tenant_workflow;
ALTER TABLE obligations DROP COLUMN IF EXISTS workflow_id;

COMMIT;