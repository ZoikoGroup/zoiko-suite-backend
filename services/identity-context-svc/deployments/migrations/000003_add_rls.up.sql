-- Migration: 000003_add_rls.up.sql
--
-- Closes the gap 000001's own comment left open: no row-level security
-- existed on any table in this service, and PrincipalStore's read/write
-- methods (FindByID, FindActiveRoleAssignments, FindActiveDelegations,
-- UpdateStatus) did not carry tenant_id through the interface at all, so
-- there was no tenant value to enforce a policy against even at the
-- application layer — GET /v1/principals/{id} and its sibling routes had
-- no tenant scoping of any kind, reachable through the documented happy
-- path with just a principal_id.
--
-- The application-layer fix (PrincipalStore now takes tenantID on every
-- method; the handler requires X-Tenant-Id) lands in the same change as
-- this migration. This policy is the defense-in-depth backstop, per the
-- same doctrine as governance-decision-log-svc's 000002/000006: FORCE from
-- the start (this service has no pre-existing traffic to migrate), and
-- WITH CHECK stated explicitly rather than relied on implicitly via USING.
--
-- current_setting('app.tenant_id', true) — missing_ok = true — returns
-- NULL rather than raising when unset, so a connection that forgot to set
-- the scope matches no rows instead of erroring.
--
-- tenant_id is VARCHAR here (principal_id and friends are ULIDs, not
-- UUIDs — see 000001's own note), so no ::UUID cast, unlike
-- tenant-entity-registry-svc's policies.

ALTER TABLE principals ENABLE ROW LEVEL SECURITY;
ALTER TABLE principals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON principals
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE principal_role_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE principal_role_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON principal_role_assignments
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE delegated_authorities ENABLE ROW LEVEL SECURITY;
ALTER TABLE delegated_authorities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON delegated_authorities
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE access_decision_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_decision_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON access_decision_log
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
