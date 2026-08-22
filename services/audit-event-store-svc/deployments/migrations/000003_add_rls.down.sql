-- Migration: 000003_add_rls.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON audit_events;
ALTER TABLE audit_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events DISABLE ROW LEVEL SECURITY;
