-- Migration: 000003_add_rls.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON secret_access_audit_log;
ALTER TABLE secret_access_audit_log NO FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_access_audit_log DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON secret_leases;
ALTER TABLE secret_leases NO FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_leases DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON secret_policy_versions;
ALTER TABLE secret_policy_versions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE secret_policy_versions DISABLE ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS secret_vault_platform_scope();
