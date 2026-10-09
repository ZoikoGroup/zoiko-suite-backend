-- Idempotency-Key is honoured, not merely demanded (Group 1 cross-service
-- finding 2). The envelope middleware has required the header on every write
-- here since the contract rollout, and nothing read it: a retried
-- POST /v1/notifications/{id}/resend recorded a second reasoned resend, and a
-- retried template approval answered 409 on the second call because the first
-- had succeeded. Same table shape as identity-context-svc and
-- search-indexer-svc, which the audit names as the pattern to copy.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(255) NOT NULL,
    endpoint            VARCHAR(512) NOT NULL,
    idempotency_key     VARCHAR(255) NOT NULL,
    -- sha256(principal NUL body): a second principal presenting the first
    -- one's key is a different request, not a replay of someone else's answer.
    request_fingerprint VARCHAR(64)  NOT NULL,
    -- 0 while the command is in flight.
    response_status     INTEGER      NOT NULL DEFAULT 0,
    response_body       JSONB        NOT NULL DEFAULT 'null'::jsonb,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, endpoint, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created ON idempotency_keys (created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idempotency_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_tenant_isolation ON idempotency_keys FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
