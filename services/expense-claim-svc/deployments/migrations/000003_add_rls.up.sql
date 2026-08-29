ALTER TABLE expense_claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE expense_claims FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON expense_claims
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE expense_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE expense_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON expense_lines
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE expense_claim_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE expense_claim_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON expense_claim_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
