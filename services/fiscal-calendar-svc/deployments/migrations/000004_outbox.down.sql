DROP POLICY IF EXISTS outbox_tenant_isolation ON fiscal_calendar_outbox;
DROP INDEX IF EXISTS idx_fiscal_calendar_outbox_unpublished;
DROP TABLE IF EXISTS fiscal_calendar_outbox;
