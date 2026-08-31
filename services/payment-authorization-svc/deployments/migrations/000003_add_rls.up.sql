ALTER TABLE payment_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_authorizations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payment_authorizations
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE authorization_payee_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE authorization_payee_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON authorization_payee_snapshots
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE authorization_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE authorization_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON authorization_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
