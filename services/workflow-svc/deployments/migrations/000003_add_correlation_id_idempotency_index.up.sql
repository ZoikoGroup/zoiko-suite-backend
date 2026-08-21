-- workflow_instances.correlation_id has existed since 000001 but was never
-- backed by a uniqueness constraint, so a retried POST /v1/workflows (e.g.
-- a client timeout followed by a retry) creates a second, fully duplicate
-- workflow instance — complete with its own duplicate stage chain and
-- initial PENDING transition. Doc 03 §3.7 names "approval action handling"
-- explicitly as one of the platform's mandatory idempotency cases; this was
-- the one real gap against it in this service.
--
-- Partial (WHERE correlation_id != '') so requests made with no correlation
-- ID (the empty-string default when the caller sends no X-Correlation-ID
-- header) are never treated as colliding with one another — same idiom
-- used platform-wide (general-ledger-svc, payroll-run-svc, etc.).
CREATE UNIQUE INDEX idx_workflow_instances_correlation_unique
    ON workflow_instances (tenant_id, correlation_id)
    WHERE correlation_id != '';
