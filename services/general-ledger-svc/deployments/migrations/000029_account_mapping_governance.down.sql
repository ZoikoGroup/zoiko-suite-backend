-- Migration: 000029_account_mapping_governance.down.sql

DROP INDEX IF EXISTS idx_account_mappings_superseded_by;
DROP INDEX IF EXISTS idx_account_mappings_active;

CREATE UNIQUE INDEX idx_account_mappings_current
    ON account_mappings (tenant_id, mapping_key)
    WHERE effective_to IS NULL;

ALTER TABLE account_mappings
    DROP CONSTRAINT IF EXISTS account_mappings_status_check,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS is_high_risk,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approved_by_principal_id,
    DROP COLUMN IF EXISTS activated_at,
    DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS superseded_by_mapping_id;
