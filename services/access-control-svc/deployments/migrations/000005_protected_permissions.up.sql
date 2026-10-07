-- Migration 000005: Protected permissions table
-- Platform-admin permissions that tenant roles may NOT include (per Authorization Standard §9)

CREATE TABLE IF NOT EXISTS protected_permissions (
    action_name         VARCHAR(200) PRIMARY KEY,
    description         TEXT NOT NULL,
    category            VARCHAR(100) NOT NULL, -- e.g., 'platform_admin', 'system_control'
    active_flag         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

-- Enable RLS (tenant-scoped read for governance visibility; writes are system-only)
ALTER TABLE protected_permissions ENABLE ROW LEVEL SECURITY;

-- Multi-tenant read policy (all tenants can see the list)
-- protected_permissions is a PLATFORM-WIDE catalogue: it has no tenant_id, and
-- an earlier version of this policy compared one, so the migration failed with
-- "column tenant_id does not exist" on every database. Every tenant reads the
-- same catalogue, and nothing may write to it through RLS (there is no INSERT,
-- UPDATE or DELETE policy), so reads are open and writes are refused.
DROP POLICY IF EXISTS tenant_isolation_policy ON protected_permissions;
DROP POLICY IF EXISTS protected_permissions_read ON protected_permissions;
CREATE POLICY protected_permissions_read ON protected_permissions FOR SELECT
    USING (true);

-- Seed the 8 baseline protected actions from the static conflict matrix (§10.1)
-- These mirror authorization-svc's internal protected set
INSERT INTO protected_permissions (action_name, description, category) VALUES
    ('PLATFORM_ADMIN', 'Full platform administration', 'platform_admin'),
    ('TENANT_ADMIN', 'Tenant-wide administration', 'platform_admin'),
    ('ROLE_MANAGE', 'Manage role definitions and bundles', 'platform_admin'),
    ('USER_PROVISION', 'Provision/deprovision users', 'platform_admin'),
    ('ENTITY_MANAGE', 'Manage legal entities', 'platform_admin'),
    ('AUDIT_READ', 'Read audit logs across tenants', 'platform_admin'),
    ('SECURITY_POLICY_MANAGE', 'Manage security policies', 'platform_admin'),
    ('BILLING_ADMIN', 'Manage billing and subscriptions', 'platform_admin')
ON CONFLICT (action_name) DO UPDATE SET
    description = EXCLUDED.description,
    category = EXCLUDED.category,
    active_flag = EXCLUDED.active_flag,
    updated_at = now();

-- Index for fast lookup during validation
CREATE INDEX IF NOT EXISTS idx_protected_permissions_active ON protected_permissions (action_name) WHERE active_flag = TRUE;