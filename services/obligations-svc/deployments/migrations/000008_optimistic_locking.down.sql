-- 000008_optimistic_locking.down.sql
-- Obligations Service — optimistic locking rollback

BEGIN;

ALTER TABLE obligations DROP COLUMN IF EXISTS version;

COMMIT;