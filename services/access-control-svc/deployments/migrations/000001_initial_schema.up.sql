-- Initial schema for Access Control Service (access-control-svc)
--
-- Scope: this service owns role/permission-bundle DEFINITIONS (the
-- catalogue). It does not own per-principal role ASSIGNMENTS — those remain
-- in authorization-svc. See internal/domain/types.go for the full scope
-- rationale.

CREATE TABLE IF NOT EXISTS role_definitions (
    role_definition_id       UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    role_code                VARCHAR(100) NOT NULL,
    role_name                VARCHAR(255) NOT NULL,
    role_scope_type          VARCHAR(30) NOT NULL, -- LEGAL_ENTITY, TENANT
    status                   VARCHAR(20) NOT NULL, -- ACTIVE, RETIRED
    created_by_principal_id  VARCHAR(255) NOT NULL,
    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    UNIQUE (tenant_id, role_code)
);

CREATE TABLE IF NOT EXISTS permission_bundle_defs (
    bundle_id                UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    role_definition_id       UUID NOT NULL REFERENCES role_definitions(role_definition_id) ON DELETE CASCADE,
    bundle_code              VARCHAR(100) NOT NULL,
    permitted_actions        TEXT[] NOT NULL,
    active_flag              BOOLEAN NOT NULL DEFAULT TRUE,
    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE role_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE permission_bundle_defs ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_policy ON role_definitions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON permission_bundle_defs FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried create with the same (tenant_id, correlation_id)
-- must resolve to the original record, never a duplicate.
CREATE UNIQUE INDEX idx_role_definitions_tenant_correlation ON role_definitions (tenant_id, correlation_id);
CREATE UNIQUE INDEX idx_permission_bundle_defs_tenant_correlation ON permission_bundle_defs (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_permission_bundle_defs_tenant_role ON permission_bundle_defs (tenant_id, role_definition_id);
