-- Idempotency-Key replay store.
--
-- The platform envelope requires an Idempotency-Key on every material write;
-- this table lets the service answer a retried write from its first response
-- instead of running it twice (2026-10-27 live e2e: a retried create made a
-- second record). Read/written only by internal/idempotency.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(255) NOT NULL,
    endpoint            VARCHAR(500) NOT NULL,   -- "METHOD /path?query"
    idempotency_key     VARCHAR(255) NOT NULL,
    request_fingerprint TEXT         NOT NULL,   -- sha256(principal || 0x00 || body)
    response_status     INT          NOT NULL DEFAULT 0,  -- 0 = first request still running
    response_body       BYTEA,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, endpoint, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created_at ON idempotency_keys (created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- mtls-management-svc: provisioning and rotation answers carry the leaf
-- PRIVATE KEY, which this service never retains. Such an answer is recorded
-- with secret_withheld = true and NO body, so a replay is refused (409
-- idempotency_secret_not_replayable) instead of re-issuing or storing the key.
ALTER TABLE idempotency_keys ADD COLUMN IF NOT EXISTS secret_withheld BOOLEAN NOT NULL DEFAULT false;
