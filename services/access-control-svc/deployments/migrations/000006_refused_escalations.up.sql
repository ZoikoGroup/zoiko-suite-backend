-- Migration 000006: Refused escalations table
-- Durable record of every pre-provisioning refusal (409, 400, 403) for evidence/audit
-- Per Doc 04 §20 "denials are as important as grants"

CREATE TABLE IF NOT EXISTS refused_escalations (
    refused_escalation_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    principal_id            VARCHAR(255) NOT NULL,        -- the caller
    correlation_id          VARCHAR(255) NOT NULL,        -- the request's idempotency key
    action_type             VARCHAR(100) NOT NULL,        -- 'CREATE_ROLE', 'UPDATE_ROLE', 'CREATE_BUNDLE', 'UPDATE_BUNDLE', 'DETACH_BUNDLE'
    refusal_reason          VARCHAR(50) NOT NULL,         -- 'role_code_exists', 'bundle_code_exists', 'protected_action', 'sod_conflict', 'forbidden', 'invalid_request', 'identity_missing', 'tenant_missing', 'authz_unavailable', 'authz_admin_unavailable'
    requested_payload       JSONB NOT NULL,               -- what the caller tried to do
    error_code              VARCHAR(50) NOT NULL,         -- the error_code returned to caller
    error_message           TEXT NOT NULL,                -- the error_message returned to caller
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

-- Enable Row-Level Security
ALTER TABLE refused_escalations ENABLE ROW LEVEL SECURITY;

-- Multi-tenant isolation
CREATE POLICY tenant_isolation_policy ON refused_escalations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Relay policy for outbox drainer (cross-tenant read for monitoring)
CREATE POLICY relay_policy ON refused_escalations FOR SELECT
    USING (current_setting('app.outbox_relay', true) = 'true');

-- Indexes for common queries
CREATE INDEX idx_refused_escalations_tenant_time ON refused_escalations (tenant_id, created_at DESC);
CREATE INDEX idx_refused_escalations_correlation ON refused_escalations (tenant_id, correlation_id);
CREATE INDEX idx_refused_escalations_reason ON refused_escalations (tenant_id, refusal_reason, created_at DESC);
CREATE INDEX idx_refused_escalations_principal ON refused_escalations (tenant_id, principal_id, created_at DESC);