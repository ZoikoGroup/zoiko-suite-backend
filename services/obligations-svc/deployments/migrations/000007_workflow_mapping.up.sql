-- 000007_workflow_mapping.up.sql
-- Obligations Service — obligation-to-workflow mappings (03-microservices.md §8.5)
--
-- Spec §8.5: "Owns: obligation-to-workflow mappings"
-- Adds workflow_id column to link obligations to workflow & approvals service instances.

BEGIN;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS workflow_id UUID;

-- Index for querying obligations by workflow
CREATE INDEX IF NOT EXISTS idx_obligations_tenant_workflow
    ON obligations (tenant_id, workflow_id);

COMMIT;