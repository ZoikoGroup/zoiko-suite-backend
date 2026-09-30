-- 000016_add_workflow_definitions.down.sql
-- Rollback workflow definitions

ALTER TABLE workflow_instances
    DROP COLUMN IF EXISTS workflow_definition_id,
    DROP COLUMN IF EXISTS workflow_definition_version;

DROP TABLE IF EXISTS workflow_definitions;