-- Initial schema for Delegated Authority Service (delegated-authority-svc)

CREATE TABLE IF NOT EXISTS delegation_grants (
    delegation_id               UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id              VARCHAR(255) NOT NULL,
    delegator_principal_id      VARCHAR(255) NOT NULL,
    delegate_principal_id       VARCHAR(255) NOT NULL,
    action_type                 VARCHAR(100) NOT NULL,
    effective_from               TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to                 TIMESTAMP WITH TIME ZONE NOT NULL,
    status                      VARCHAR(20) NOT NULL, -- ACTIVE, REVOKED, EXPIRED
    created_by_principal_id     VARCHAR(255) NOT NULL,
    correlation_id              VARCHAR(255) NOT NULL,
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                  TIMESTAMP WITH TIME ZONE NOT NULL,
    revoked_by_principal_id     VARCHAR(255),
    revoked_at                  TIMESTAMP WITH TIME ZONE,
    expired_at                  TIMESTAMP WITH TIME ZONE,
    CHECK (effective_to > effective_from)
);

-- Enable Row-Level Security
ALTER TABLE delegation_grants ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policy
CREATE POLICY tenant_isolation_policy ON delegation_grants FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried create with the same (tenant_id, correlation_id)
-- must resolve to the original record, never a duplicate.
CREATE UNIQUE INDEX idx_delegation_grants_tenant_correlation ON delegation_grants (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_delegation_grants_tenant_entity_status ON delegation_grants (tenant_id, legal_entity_id, status);
CREATE INDEX idx_delegation_grants_tenant_delegate ON delegation_grants (tenant_id, delegate_principal_id, status);

-- Supports the lazy-expiry sweep's WHERE clause (status = 'ACTIVE' AND
-- effective_to < now()).
CREATE INDEX idx_delegation_grants_tenant_active_effective_to ON delegation_grants (tenant_id, effective_to) WHERE status = 'ACTIVE';
