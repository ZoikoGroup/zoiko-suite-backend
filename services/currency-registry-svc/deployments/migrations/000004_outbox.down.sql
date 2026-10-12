DROP POLICY IF EXISTS outbox_tenant_isolation ON currency_outbox;
DROP INDEX IF EXISTS idx_currency_outbox_unpublished;
DROP TABLE IF EXISTS currency_outbox;
