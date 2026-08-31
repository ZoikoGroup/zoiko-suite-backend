DROP POLICY IF EXISTS tenant_isolation ON status_events;
ALTER TABLE status_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE status_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payment_execution_states;
ALTER TABLE payment_execution_states NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_execution_states DISABLE ROW LEVEL SECURITY;
