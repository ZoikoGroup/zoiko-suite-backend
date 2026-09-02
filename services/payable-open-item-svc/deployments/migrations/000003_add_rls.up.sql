ALTER TABLE payable_open_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE payable_open_items FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payable_open_items
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE settlement_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE settlement_applications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON settlement_applications
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
