-- 000002_add_idempotency.up.sql
--
-- correlation_id columns + partial unique indexes so retried
-- CreateDepartment/CreatePosition/AssignEmployee calls resolve to the
-- ORIGINAL row instead of creating a duplicate.
ALTER TABLE departments ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_departments_tenant_correlation
    ON departments (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE positions ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_positions_tenant_correlation
    ON positions (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE org_assignments ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_org_assignments_tenant_correlation
    ON org_assignments (tenant_id, correlation_id)
    WHERE correlation_id != '';
