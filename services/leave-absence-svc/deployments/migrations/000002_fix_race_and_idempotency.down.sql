DROP INDEX IF EXISTS idx_leave_requests_tenant_correlation;
ALTER TABLE leave_requests DROP COLUMN IF EXISTS correlation_id;
