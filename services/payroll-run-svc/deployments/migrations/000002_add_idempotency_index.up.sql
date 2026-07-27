-- Migration: 000002_add_idempotency_index.up.sql
--
-- Adds a partial unique index on (tenant_id, correlation_id) so a retried
-- POST /v1/payroll/runs (e.g. a client timeout on a request that actually
-- succeeded server-side) resolves to the ORIGINAL run instead of creating
-- a duplicate payroll run for the same period.
ALTER TABLE payroll_runs ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_payroll_runs_tenant_correlation
    ON payroll_runs (tenant_id, correlation_id)
    WHERE correlation_id != '';
