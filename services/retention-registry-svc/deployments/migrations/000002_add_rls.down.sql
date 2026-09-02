-- 000002_add_rls.down.sql
--
-- Rolling this back removes a tenant boundary but does NOT affect whether
-- retention rules and legal holds resolve correctly: the store's own
-- "tenant_id IS NULL OR tenant_id = $n" predicates are independent of the
-- policy. As recorded in the up migration, TIGHTENING the policy is the
-- dangerous direction here, not removing it.
DROP POLICY IF EXISTS tenant_isolation_policy ON legal_holds;
ALTER TABLE legal_holds NO FORCE ROW LEVEL SECURITY;
ALTER TABLE legal_holds DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON retention_policies;
ALTER TABLE retention_policies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE retention_policies DISABLE ROW LEVEL SECURITY;
