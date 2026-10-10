-- Migration: 000006_outbox_events.down.sql
--
-- Reverts 000006_outbox_events.up.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON outbox_events;
ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS idx_outbox_unpublished;
DROP INDEX IF EXISTS idx_outbox_aggregate;
DROP INDEX IF EXISTS idx_outbox_tenant;
DROP TABLE IF EXISTS outbox_events;