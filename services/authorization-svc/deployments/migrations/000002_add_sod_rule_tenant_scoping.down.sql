DROP INDEX IF EXISTS idx_sod_rules_tenant;
ALTER TABLE sod_rules DROP COLUMN IF EXISTS tenant_id;
