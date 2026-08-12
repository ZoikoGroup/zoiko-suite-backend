-- 000005_add_workspaces.up.sql
--
-- docs/original_doc/zoiko_suite_doc7.txt §A5/§T: workspaces sit beneath a
-- tenant (= this platform's "organization") and can legitimately be
-- non-billable (pilots, internal controls, sandboxes). billing_classification
-- and billing_source are the commercial facts the spec requires on every
-- workspace, even though the commercial_account they may reference lives in
-- a separate service (commercial-account-svc) per the Five-Plane Trust
-- Doctrine's explicit Plane 1/Plane 2 separation (doc7 §3) — so
-- commercial_account_id below is a plain nullable string reference, not a
-- cross-database FK, same pattern this platform already uses for
-- legal_entity_id references from other services.

CREATE TABLE workspaces (
    workspace_id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES tenants(tenant_id),
    legal_entity_id UUID REFERENCES legal_entities(legal_entity_id),
    name VARCHAR(255) NOT NULL,
    business_unit VARCHAR(255),
    billing_classification VARCHAR(50) NOT NULL,
    billing_source VARCHAR(50) NOT NULL DEFAULT 'NONE',
    commercial_account_id UUID,
    status VARCHAR(50) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON workspaces
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

CREATE INDEX idx_workspaces_tenant ON workspaces (tenant_id);
