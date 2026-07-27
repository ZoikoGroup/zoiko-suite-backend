DROP INDEX IF EXISTS idx_work_logs_tenant_correlation;
ALTER TABLE working_hour_logs DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_visa_records_tenant_correlation;
ALTER TABLE visa_records DROP COLUMN IF EXISTS correlation_id;

DROP INDEX IF EXISTS idx_work_auth_tenant_correlation;
ALTER TABLE work_authorizations DROP COLUMN IF EXISTS correlation_id;
