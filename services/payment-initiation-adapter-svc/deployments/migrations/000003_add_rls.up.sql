ALTER TABLE payment_initiation_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_initiation_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payment_initiation_attempts
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE attempt_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE attempt_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON attempt_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
