-- 000006_add_missing_schema_fields.down.sql
-- Revert 000006_add_missing_schema_fields.up.sql

-- Restore the original dedup index
DROP INDEX IF EXISTS idx_policy_versions_dedup;
CREATE UNIQUE INDEX idx_policy_versions_dedup ON policy_versions (
    policy_id,
    COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
    COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
    effective_from
);

-- Drop added columns from policy_versions
ALTER TABLE policy_versions
    DROP COLUMN IF EXISTS version_number,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS rationale,
    DROP COLUMN IF EXISTS artifact_digest,
    DROP COLUMN IF EXISTS known_from;

-- Drop added columns from policies
ALTER TABLE policies
    DROP COLUMN IF EXISTS tenant_id,
    DROP COLUMN IF EXISTS policy_status,
    DROP COLUMN IF EXISTS versioning_mode;

-- Drop index
DROP INDEX IF EXISTS idx_policies_tenant;