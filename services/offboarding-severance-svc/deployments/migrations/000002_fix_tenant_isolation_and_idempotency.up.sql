-- 000002_fix_tenant_isolation_and_idempotency.up.sql
--
-- correlation_id columns + partial unique indexes so a retried
-- POST /v1/terminations or POST /v1/offboarding/checklists resolves to the
-- original row instead of creating a duplicate termination request or
-- checklist (audit finding: no idempotency protection existed at all).
ALTER TABLE termination_requests ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_term_req_tenant_correlation
    ON termination_requests (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE offboarding_checklists ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_offboard_chk_tenant_correlation
    ON offboarding_checklists (tenant_id, correlation_id)
    WHERE correlation_id != '';
