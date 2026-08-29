DROP POLICY IF EXISTS tenant_isolation ON receipt_accounting_events;
ALTER TABLE receipt_accounting_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE receipt_accounting_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON receipt_reversals;
ALTER TABLE receipt_reversals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE receipt_reversals DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON receipt_evidence;
ALTER TABLE receipt_evidence NO FORCE ROW LEVEL SECURITY;
ALTER TABLE receipt_evidence DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON goods_service_receipts;
ALTER TABLE goods_service_receipts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE goods_service_receipts DISABLE ROW LEVEL SECURITY;
