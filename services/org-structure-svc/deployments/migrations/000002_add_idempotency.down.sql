DROP INDEX IF EXISTS idx_org_assignments_tenant_correlation;
ALTER TABLE org_assignments DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_positions_tenant_correlation;
ALTER TABLE positions DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_departments_tenant_correlation;
ALTER TABLE departments DROP COLUMN IF EXISTS correlation_id;
