DROP INDEX IF EXISTS idx_bonus_grants_tenant_correlation;
ALTER TABLE bonus_grants DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_wage_rev_tenant_correlation;
ALTER TABLE wage_revisions DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_comp_struct_tenant_correlation;
ALTER TABLE compensation_structures DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_wage_revisions_one_active;
