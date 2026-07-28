DROP INDEX IF EXISTS idx_offboard_chk_tenant_correlation;
ALTER TABLE offboarding_checklists DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_term_req_tenant_correlation;
ALTER TABLE termination_requests DROP COLUMN IF EXISTS correlation_id;
