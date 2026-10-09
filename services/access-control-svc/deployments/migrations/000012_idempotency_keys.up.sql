-- Migration 000012: Idempotency-Key is honoured, not only required.
--
-- The envelope contract has required Idempotency-Key on every write here since
-- the service was built, and nothing read it. Writes were idempotent on the
-- body's correlation_id instead, so a retry that kept the header but changed
-- correlation_id was a second write, and the same key reused for a different
-- request was accepted silently. This is the same store identity-context-svc
-- uses (its 000008): the key is bound to a fingerprint of the caller and the
-- body, a mismatch is refused 409 IDEMPOTENCY_MISMATCH, a retry is answered
-- from the first response.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id            VARCHAR(255) NOT NULL,
    endpoint             VARCHAR(512) NOT NULL,
    idempotency_key      VARCHAR(255) NOT NULL,
    request_fingerprint  CHAR(64)     NOT NULL,
    -- 0 while the command is in flight.
    response_status      INTEGER      NOT NULL DEFAULT 0,
    response_body        JSONB        NOT NULL DEFAULT 'null'::jsonb,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, endpoint, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created ON idempotency_keys (created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idempotency_keys_tenant ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant ON idempotency_keys FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
