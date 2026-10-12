DROP POLICY IF EXISTS tenant_isolation_policy ON workflow_history_events;
ALTER TABLE workflow_history_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_history_events DISABLE ROW LEVEL SECURITY;

