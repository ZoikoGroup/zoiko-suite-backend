ALTER TABLE payment_execution_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_execution_states FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payment_execution_states
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE status_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE status_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON status_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
