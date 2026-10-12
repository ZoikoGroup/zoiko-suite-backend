ALTER TABLE receipt_accounting_events DROP CONSTRAINT IF EXISTS chk_receipt_accounting_events_tenant_not_null;
ALTER TABLE receipt_reversals DROP CONSTRAINT IF EXISTS chk_receipt_reversals_tenant_not_null;
ALTER TABLE receipt_evidence DROP CONSTRAINT IF EXISTS chk_receipt_evidence_tenant_not_null;
ALTER TABLE goods_service_receipts DROP CONSTRAINT IF EXISTS chk_gsr_tenant_not_null;

DROP POLICY IF EXISTS tenant_isolation ON receipt_accounting_events;
CREATE POLICY tenant_isolation ON receipt_accounting_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
DROP POLICY IF EXISTS tenant_isolation ON receipt_reversals;
CREATE POLICY tenant_isolation ON receipt_reversals
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
DROP POLICY IF EXISTS tenant_isolation ON receipt_evidence;
CREATE POLICY tenant_isolation ON receipt_evidence
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
DROP POLICY IF EXISTS tenant_isolation ON goods_service_receipts;
CREATE POLICY tenant_isolation ON goods_service_receipts
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
