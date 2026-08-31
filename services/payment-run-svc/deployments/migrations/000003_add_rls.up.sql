ALTER TABLE payment_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payment_runs
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE run_instructions ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_instructions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON run_instructions
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE instruction_reconciliation_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE instruction_reconciliation_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON instruction_reconciliation_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE run_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON run_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
