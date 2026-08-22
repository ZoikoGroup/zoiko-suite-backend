DROP POLICY IF EXISTS tenant_isolation_policy ON automation_actions;
ALTER TABLE automation_actions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE automation_actions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON automation_policies;
ALTER TABLE automation_policies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE automation_policies DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON ai_runs;
ALTER TABLE ai_runs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE ai_runs DISABLE ROW LEVEL SECURITY;
