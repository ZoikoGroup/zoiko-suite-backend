-- Migration: 000003_add_rls.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON access_decision_log;
ALTER TABLE access_decision_log NO FORCE ROW LEVEL SECURITY;
ALTER TABLE access_decision_log DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON delegated_authorities;
ALTER TABLE delegated_authorities NO FORCE ROW LEVEL SECURITY;
ALTER TABLE delegated_authorities DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON principal_role_assignments;
ALTER TABLE principal_role_assignments NO FORCE ROW LEVEL SECURITY;
ALTER TABLE principal_role_assignments DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON principals;
ALTER TABLE principals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE principals DISABLE ROW LEVEL SECURITY;
