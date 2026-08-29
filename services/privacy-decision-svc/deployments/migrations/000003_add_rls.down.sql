DROP POLICY IF EXISTS tenant_isolation_policy ON privacy_decisions;
ALTER TABLE privacy_decisions DISABLE ROW LEVEL SECURITY;
