DROP POLICY IF EXISTS outbox_tenant_isolation ON accounting_period_outbox;
DROP INDEX IF EXISTS idx_accounting_period_outbox_unpublished;
DROP TABLE IF EXISTS accounting_period_outbox;
