-- 000009_idempotency_keys.up.sql
-- Obligations Service — idempotency key tracking for write deduplication
--
-- Platform standard §3.7: Every state-changing API must be idempotent.
-- §2.1: Idempotency-Key header mandatory on writes, used for deduplication.
-- This table tracks processed idempotency keys with TTL-based cleanup.

BEGIN;

CREATE TABLE idempotency_keys (
    idempotency_key   TEXT        NOT NULL,
    tenant_id         UUID        NOT NULL,
    -- The hash of the request body to detect replay with different payload
    request_hash      TEXT        NOT NULL,
    -- The resource ID created by this request (for replay response)
    resource_id       UUID        NOT NULL,
    -- The resource type (e.g., "obligation", "filing_requirement", "applicability_decision")
    resource_type     VARCHAR(64) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

-- Index for TTL cleanup (keys older than 24h can be removed)
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created_at
    ON idempotency_keys (created_at);

-- RLS for idempotency_keys
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON idempotency_keys;
CREATE POLICY tenant_isolation_policy ON idempotency_keys FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;