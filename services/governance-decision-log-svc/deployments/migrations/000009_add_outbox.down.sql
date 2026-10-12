-- Migration: 000009_add_outbox.down.sql
-- Drops all objects created by 000009_add_outbox.up.sql.

DROP POLICY IF EXISTS tenant_isolation_policy ON outbox;
ALTER TABLE outbox DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_outbox_unpublished;
DROP TABLE IF EXISTS outbox;