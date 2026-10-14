-- 000009_idempotency_keys.down.sql
-- Obligations Service — idempotency key tracking rollback

BEGIN;

DROP INDEX IF EXISTS idx_idempotency_keys_created_at;
DROP TABLE IF EXISTS idempotency_keys;

COMMIT;