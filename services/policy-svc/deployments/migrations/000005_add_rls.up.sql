-- Migration: 000005_add_rls.up.sql
--
-- policies, control_test_definitions, control_test_executions, and
-- attestations have no tenant_id at all (deliberate — see each table's
-- own comment in earlier migrations: they are platform-wide reference/
-- catalogue data, same doctrine as jurisdiction-rules-svc's jurisdictions),
-- so none of them gets a policy here. policy_versions carries a nullable
-- tenant_id (NULL = global scope, a real and intentional state, not a
-- missing value) and gets a tenant_isolation_policy allowing a row when
-- either its own tenant_id is NULL or it matches the session's tenant.
--
-- No platform-scope bypass needed here, unlike secret-vault-integration-
-- svc's equivalent migration: ActivateVersion authorizes against the
-- target version's OWN legal_entity_id (handler.go), never a blanket
-- platform-wide action, so activation always happens within whatever
-- tenant scope the version was already fetched under.
--
-- current_setting(..., true) — missing_ok = true — returns NULL rather
-- than raising when app.tenant_id is unset, so a connection that forgot
-- to set it matches no tenant-scoped rows instead of erroring.

ALTER TABLE policy_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON policy_versions
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
