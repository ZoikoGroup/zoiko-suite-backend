DROP INDEX IF EXISTS idx_delegation_grants_tenant_created;

ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_delegate_differs;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_expired_has_evidence;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_revoked_has_evidence;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_status_known;

DROP POLICY IF EXISTS tenant_isolation_policy ON delegation_grants;
CREATE POLICY tenant_isolation_policy ON delegation_grants FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE delegation_grants NO FORCE ROW LEVEL SECURITY;
