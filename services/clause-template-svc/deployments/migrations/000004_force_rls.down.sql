DROP POLICY IF EXISTS clause_versions_tenant_isolation ON clause_versions;
ALTER TABLE clause_versions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE clause_versions DISABLE ROW LEVEL SECURITY;
