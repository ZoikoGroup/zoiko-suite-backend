-- 000010_event_outbox.down.sql
-- Obligations Service — transactional outbox rollback

BEGIN;

DROP INDEX IF EXISTS idx_event_outbox_pending;
DROP TABLE IF EXISTS event_outbox;

COMMIT;