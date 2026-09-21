-- 000011_add_outbox_events.down.sql
DROP INDEX IF EXISTS idx_outbox_events_tenant_entity;
DROP INDEX IF EXISTS idx_outbox_events_unpublished;
DROP TABLE IF EXISTS outbox_events;
