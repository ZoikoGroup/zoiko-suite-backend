-- Migration: 000014_replay_idempotency.up.sql
--
-- POST /v1/decisions/{id}/replay minted a new manifest id on every call and
-- ignored Idempotency-Key, so a client retrying a timed-out replay wrote a
-- second permanent, append-only manifest for the same request. A replay is now
-- idempotent on (tenant_id, decision_id, idempotency_key): the retry gets the
-- manifest the first call wrote, and the same key with a different body is
-- refused (API standard: same key, changed request -> 409).
--
-- Adding nullable columns does not touch existing rows, so the append-only
-- trigger is not involved. Manifests written before this migration have no
-- key and are never matched.

BEGIN;

ALTER TABLE replay_manifests
    ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(256),
    ADD COLUMN IF NOT EXISTS request_hash    BYTEA;

CREATE UNIQUE INDEX IF NOT EXISTS uq_replay_manifests_idempotency
    ON replay_manifests (tenant_id, decision_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

COMMIT;
