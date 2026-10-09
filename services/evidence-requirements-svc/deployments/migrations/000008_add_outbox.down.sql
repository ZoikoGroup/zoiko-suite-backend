-- Migration: 000008_add_outbox.down.sql
-- Revert outbox changes

DROP FUNCTION IF EXISTS mark_outbox_failed(UUID[], TEXT);
DROP FUNCTION IF EXISTS mark_outbox_published(UUID[]);
DROP FUNCTION IF EXISTS claim_outbox_events(INT);
DROP TABLE IF EXISTS outbox;