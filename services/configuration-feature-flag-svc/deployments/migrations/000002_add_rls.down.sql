-- Migration: 000002_add_rls.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON feature_flags;
ALTER TABLE feature_flags NO FORCE ROW LEVEL SECURITY;
ALTER TABLE feature_flags DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON config_entries;
ALTER TABLE config_entries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE config_entries DISABLE ROW LEVEL SECURITY;
