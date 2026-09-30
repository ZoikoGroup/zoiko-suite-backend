DROP POLICY IF EXISTS outbox_tenant_isolation ON event_outbox;
DROP INDEX IF EXISTS idx_event_outbox_unpublished;
DROP TABLE IF EXISTS event_outbox;
