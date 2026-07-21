-- 000002_fix_tenant_isolation_and_idempotency.up.sql
--
-- correlation_id columns + partial unique indexes so a retried
-- POST /v1/compliance/work-auth, /visas, or /hours resolves to the
-- original row instead of creating a duplicate (audit finding: no
-- idempotency protection existed at all — for /hours specifically, a
-- retried call would double-count an employee's weekly accumulated
-- hours, which feeds directly into a statutory breach determination).
ALTER TABLE work_authorizations ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_work_auth_tenant_correlation
    ON work_authorizations (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE visa_records ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_visa_records_tenant_correlation
    ON visa_records (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE working_hour_logs ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_work_logs_tenant_correlation
    ON working_hour_logs (tenant_id, correlation_id)
    WHERE correlation_id != '';
