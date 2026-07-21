-- 000002_fix_lookup_and_idempotency.up.sql
--
-- correlation_id columns + partial unique indexes so a retried create
-- call resolves to the ORIGINAL row instead of creating a duplicate
-- benefit plan or election.
ALTER TABLE benefit_plans ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_benefit_plans_tenant_correlation
    ON benefit_plans (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE benefit_elections ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_benefit_elections_tenant_correlation
    ON benefit_elections (tenant_id, correlation_id)
    WHERE correlation_id != '';
