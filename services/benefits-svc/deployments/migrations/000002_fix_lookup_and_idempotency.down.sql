DROP INDEX IF EXISTS idx_benefit_elections_tenant_correlation;
ALTER TABLE benefit_elections DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_benefit_plans_tenant_correlation;
ALTER TABLE benefit_plans DROP COLUMN IF EXISTS correlation_id;
