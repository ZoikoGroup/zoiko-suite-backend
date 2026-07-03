-- 000002_add_audit_columns.down.sql
-- Revert audit update tracking columns and rule dedup unique index.

DROP INDEX IF EXISTS idx_jrules_jurisdiction_code_effective_from_unique;

ALTER TABLE jurisdiction_rules
    DROP COLUMN IF EXISTS updated_by_principal_id,
    DROP COLUMN IF EXISTS updated_at;

ALTER TABLE jurisdictions
    DROP COLUMN IF EXISTS updated_by_principal_id,
    DROP COLUMN IF EXISTS updated_at;
