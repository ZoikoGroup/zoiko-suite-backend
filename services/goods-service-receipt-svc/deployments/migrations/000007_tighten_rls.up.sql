-- Migration: 000007_tighten_rls.up.sql
--
-- Drops the `tenant_id IS NULL OR` escape hatch from every tenant policy: a row
-- without a tenant is no longer visible to anyone, and WITH CHECK refuses to
-- write one. NOT VALID check constraints additionally stop new NULL-tenant rows
-- without rewriting (or failing on) any legacy rows; the append-only triggers
-- mean such rows could not be repaired in place anyway.

DROP POLICY IF EXISTS tenant_isolation ON goods_service_receipts;
CREATE POLICY tenant_isolation ON goods_service_receipts
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON receipt_evidence;
CREATE POLICY tenant_isolation ON receipt_evidence
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON receipt_reversals;
CREATE POLICY tenant_isolation ON receipt_reversals
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON receipt_accounting_events;
CREATE POLICY tenant_isolation ON receipt_accounting_events
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE goods_service_receipts ADD CONSTRAINT chk_gsr_tenant_not_null CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE receipt_evidence ADD CONSTRAINT chk_receipt_evidence_tenant_not_null CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE receipt_reversals ADD CONSTRAINT chk_receipt_reversals_tenant_not_null CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE receipt_accounting_events ADD CONSTRAINT chk_receipt_accounting_events_tenant_not_null CHECK (tenant_id IS NOT NULL) NOT VALID;
