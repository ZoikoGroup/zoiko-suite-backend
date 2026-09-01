ALTER TABLE payee_destinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE payee_destinations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payee_destinations
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE payee_destination_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE payee_destination_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payee_destination_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
