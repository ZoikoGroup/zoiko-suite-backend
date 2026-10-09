-- Migration: 000003_add_outbox.down.sql
-- Drops all objects created by 000003_add_outbox.up.sql.

DROP INDEX IF EXISTS idx_outbox_unpublished;
DROP TABLE IF EXISTS outbox;