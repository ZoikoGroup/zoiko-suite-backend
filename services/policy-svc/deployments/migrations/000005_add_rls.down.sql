-- Migration: 000005_add_rls.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON policy_versions;
ALTER TABLE policy_versions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE policy_versions DISABLE ROW LEVEL SECURITY;
