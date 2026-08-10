-- 000002_add_rls.down.sql
DROP POLICY IF EXISTS tenant_isolation_policy ON workflow_history_events;
ALTER TABLE workflow_history_events DISABLE ROW LEVEL SECURITY;
