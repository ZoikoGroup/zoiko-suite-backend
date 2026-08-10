-- 000002_add_rls.down.sql
DROP POLICY IF EXISTS tenant_isolation_policy ON governance_decisions;
ALTER TABLE governance_decisions DISABLE ROW LEVEL SECURITY;
