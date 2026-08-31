DROP POLICY IF EXISTS tenant_isolation ON attempt_events;
ALTER TABLE attempt_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE attempt_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payment_initiation_attempts;
ALTER TABLE payment_initiation_attempts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_initiation_attempts DISABLE ROW LEVEL SECURITY;
