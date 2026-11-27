-- Reverses 000003_outbox_rls.up.sql.
DROP POLICY IF EXISTS risk_outbox_events_tenant_isolation ON risk_outbox_events;
ALTER TABLE risk_outbox_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE risk_outbox_events DISABLE ROW LEVEL SECURITY;
