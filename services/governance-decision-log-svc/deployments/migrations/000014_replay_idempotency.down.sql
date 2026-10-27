-- Migration: 000014_replay_idempotency.down.sql

BEGIN;

DROP INDEX IF EXISTS uq_replay_manifests_idempotency;
ALTER TABLE replay_manifests
    DROP COLUMN IF EXISTS request_hash,
    DROP COLUMN IF EXISTS idempotency_key;

COMMIT;
