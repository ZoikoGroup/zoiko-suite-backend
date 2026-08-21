-- Migration: 000003_add_rls.up.sql
--
-- secret_policies has no tenant_id at all (deliberate — a vault path is a
-- platform-wide address; see 000001's own comment on that table), so it
-- gets no RLS policy here. secret_policy_versions, secret_leases, and
-- secret_access_audit_log all carry a nullable tenant_id (NULL = global
-- scope, a real and intentional state per policy-svc's precedent, not a
-- missing value) and get a tenant_isolation_policy allowing a row when
-- either its own tenant_id is NULL or it matches the session's tenant.
--
-- Two administrative operations are genuinely, deliberately cross-tenant
-- by design and would break under a plain tenant-only policy:
-- ActivateVersion (SECRET_POLICY_VERSION_ACTIVATE) may activate a version
-- in ANY tenant's scope, and Rotate's mass lease revocation
-- (SECRET_ROTATE) must revoke every tenant's leases on a rotated path,
-- not just one. Both actions are already gated behind a platform-scoped
-- authorization check (an empty entity resolves to platform scope, per
-- PutSecretMaterial's documented reasoning) before the store is ever
-- called, so the policy carries a second, explicit escape hatch:
-- app.platform_scope = 'true', set ONLY by PgStore.withPlatformScope,
-- which only those two call paths use. A caller who is merely
-- tenant-authorized never reaches it. This is an auditable session flag,
-- not a role-level RLS exemption — "this connection is acting with
-- platform authority right now" is never silent.
--
-- current_setting(..., true) — missing_ok = true — returns NULL rather
-- than raising when a setting is absent, so a connection that forgot to
-- set either flag matches no tenant-scoped rows instead of erroring.

CREATE OR REPLACE FUNCTION secret_vault_platform_scope() RETURNS boolean AS $$
    SELECT current_setting('app.platform_scope', true) = 'true';
$$ LANGUAGE sql STABLE;

ALTER TABLE secret_policy_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_policy_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON secret_policy_versions
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    );

ALTER TABLE secret_leases ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_leases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON secret_leases
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    );

ALTER TABLE secret_access_audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_access_audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON secret_access_audit_log
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    );
