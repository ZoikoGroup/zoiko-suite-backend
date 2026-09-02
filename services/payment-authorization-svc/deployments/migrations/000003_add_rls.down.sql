DROP POLICY IF EXISTS tenant_isolation ON authorization_events;
ALTER TABLE authorization_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE authorization_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON authorization_payee_snapshots;
ALTER TABLE authorization_payee_snapshots NO FORCE ROW LEVEL SECURITY;
ALTER TABLE authorization_payee_snapshots DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payment_authorizations;
ALTER TABLE payment_authorizations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_authorizations DISABLE ROW LEVEL SECURITY;
