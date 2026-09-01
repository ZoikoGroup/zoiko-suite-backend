DROP POLICY IF EXISTS tenant_isolation ON payee_destination_events;
ALTER TABLE payee_destination_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payee_destination_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payee_destinations;
ALTER TABLE payee_destinations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payee_destinations DISABLE ROW LEVEL SECURITY;
