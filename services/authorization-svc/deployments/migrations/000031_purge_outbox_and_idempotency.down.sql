BEGIN;
DROP POLICY IF EXISTS idempotency_keys_purge ON idempotency_keys;
DROP FUNCTION IF EXISTS authz_purge_idempotency_keys(INTERVAL);
DROP FUNCTION IF EXISTS authz_purge_published_outbox(INTERVAL);
COMMIT;
