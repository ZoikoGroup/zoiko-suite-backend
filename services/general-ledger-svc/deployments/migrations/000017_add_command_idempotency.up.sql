-- Migration: 000017_add_command_idempotency.up.sql
--
-- ACC-01's Idempotency-Key control for the two replay-unsafe commands
-- (SPEC_DEVIATIONS A6/B12). AmendAccountVersion and ReparentAccount each
-- append a version row per call, so a retried request could write a second
-- identical version; expected_version narrows that window but is not an
-- idempotency key. command_idempotency records the key a successful command
-- ran under, so a retry of the same request replays the recorded result
-- instead of re-executing. Shape and semantics follow financial-control-svc's
-- table of the same name (internal/store/execution.go, BeginExecution).

CREATE TABLE command_idempotency (
    tenant_id       UUID NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    command         VARCHAR(32) NOT NULL,
    account_code    VARCHAR(64) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE command_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_idempotency FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON command_idempotency
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
