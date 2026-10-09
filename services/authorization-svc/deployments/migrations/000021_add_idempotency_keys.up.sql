-- 000021: Idempotency-Key is honoured, not only required.
--
-- The envelope has required Idempotency-Key on every admin write since
-- enforcement, and nothing read it: CreateRoleAssignment minted a new id per
-- call and CreateSoDRule / CreateABACRule were plain inserts, so a retried
-- request created a second grant or a second rule. The same store
-- identity-context-svc (its 000008) and access-control-svc (its 000012) use:
-- the key is bound to a fingerprint of the caller and the body, a reuse for a
-- different request is refused 409, and a retry is answered from the first
-- response.

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

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO app_authorization;
    END IF;
END
$$;
