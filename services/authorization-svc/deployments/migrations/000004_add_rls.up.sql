-- Migration: 000004_add_rls.up.sql
--
-- Only `roles` (tenant_id UUID NOT NULL) and `sod_rules` (tenant_id UUID,
-- nullable — added by 000002, NULL means a globally-applicable rule) have
-- a real tenant_id column. permission_bundles, principal_role_assignments,
-- delegated_authorities, and access_decision_log carry no tenant_id at
-- all — only legal_entity_id, and delegated_authorities not even that
-- consistently — so no policy is added for them here. Fabricating a
-- tenant_id column on tables that were never given one is a data-model
-- change, not an RLS migration; tracked separately in
-- docs/architecture/backend-completion-tracker.md (Priority 6) rather
-- than invented in this pass.
--
-- current_setting(..., true) — missing_ok = true — returns NULL rather
-- than raising when app.tenant_id is unset, so a connection that forgot
-- to set it matches no tenant-scoped rows instead of erroring.

-- NULLIF guards against app.tenant_id being set to '' (this service's
-- withRLS helper does this for a genuinely tenantless call, e.g. a
-- global SoD rule) — '' ::uuid errors rather than returning NULL, and
-- an error is not the same as "matches nothing."
--
-- roles' policy also carries an explicit app.platform_scope escape
-- hatch, set ONLY by PgStore.withPlatformScope, used ONLY by
-- FindRoleByID — the one read whose entire purpose is discovering which
-- tenant an unknown role_id belongs to, which is structurally impossible
-- to scope by tenant before you know the answer. See withPlatformScope's
-- doc comment for why this is safe: the caller (handler.go) always
-- compares the result against its own verified tenant and refuses on
-- mismatch, so this never grants access, only visibility for a decision
-- made entirely in application code. No caller reaches this flag without
-- FindRoleByID being called first, and nothing else sets it.

ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON roles
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE sod_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE sod_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON sod_rules
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
