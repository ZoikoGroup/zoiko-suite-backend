-- 000002_add_activation_audit.down.sql
-- Drops the columns added by 000002_add_activation_audit.up.sql.

ALTER TABLE policy_versions
    DROP COLUMN IF EXISTS activated_by_principal_id,
    DROP COLUMN IF EXISTS activated_at;
