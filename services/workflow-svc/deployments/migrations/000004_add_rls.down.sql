-- Migration: 000004_add_rls.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON workflow_instances;
ALTER TABLE workflow_instances NO FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_instances DISABLE ROW LEVEL SECURITY;
