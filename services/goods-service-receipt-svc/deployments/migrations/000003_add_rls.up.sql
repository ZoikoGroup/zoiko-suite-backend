ALTER TABLE goods_service_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE goods_service_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON goods_service_receipts
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE receipt_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE receipt_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON receipt_evidence
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE receipt_reversals ENABLE ROW LEVEL SECURITY;
ALTER TABLE receipt_reversals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON receipt_reversals
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE receipt_accounting_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE receipt_accounting_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON receipt_accounting_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
