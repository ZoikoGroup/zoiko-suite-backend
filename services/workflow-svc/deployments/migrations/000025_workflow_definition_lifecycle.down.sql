-- 000025_workflow_definition_lifecycle.down.sql
-- WFC-01: Workflow Definition Publication Lifecycle - DOWN

DROP TRIGGER IF EXISTS workflow_definitions_published_immutable ON workflow_definitions;
DROP FUNCTION IF EXISTS enforce_published_immutable();

DROP INDEX IF EXISTS idx_workflow_definitions_effective;
DROP INDEX IF EXISTS idx_workflow_definitions_status;
DROP INDEX IF EXISTS idx_workflow_definitions_active_version;

-- Recreate the old unique index
CREATE UNIQUE INDEX idx_workflow_definitions_active_version
    ON workflow_definitions (tenant_id, workflow_type, version)
    WHERE is_active = true;

ALTER TABLE workflow_definitions
    DROP COLUMN IF EXISTS definition_status,
    DROP COLUMN IF EXISTS checksum,
    DROP COLUMN IF EXISTS effective_from,
    DROP COLUMN IF EXISTS effective_to,
    DROP COLUMN IF EXISTS validated_by,
    DROP COLUMN IF EXISTS validated_at,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS published_by,
    DROP COLUMN IF EXISTS published_at,
    DROP COLUMN IF EXISTS emergency_allowed;

DROP TYPE IF EXISTS definition_status;