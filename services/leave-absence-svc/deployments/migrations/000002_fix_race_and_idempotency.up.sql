-- 000002_fix_race_and_idempotency.up.sql
--
-- correlation_id column + partial unique index so a retried
-- SubmitLeaveRequest call resolves to the ORIGINAL request instead of
-- creating a duplicate leave request (and double-locking pending hours).
ALTER TABLE leave_requests ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_leave_requests_tenant_correlation
    ON leave_requests (tenant_id, correlation_id)
    WHERE correlation_id != '';
