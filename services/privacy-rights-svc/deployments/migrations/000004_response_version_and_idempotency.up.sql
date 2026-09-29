-- 000004_response_version_and_idempotency.up.sql
-- privacy-rights-svc (PRV-04):
-- 1. Add response_package_version column to rights_requests for I21
--    response package versioning / invalidate-on-change.
-- 2. Idempotency storage (§18.1) with tenant isolation & RLS.

-- 1. Add response_package_version column to rights_requests
ALTER TABLE rights_requests
    ADD COLUMN IF NOT EXISTS response_package_version INT NOT NULL DEFAULT 0;

-- 2. Idempotency storage table
CREATE TABLE IF NOT EXISTS rights_idempotency_keys (
    idempotency_key    TEXT        NOT NULL,
    tenant_id          TEXT        NOT NULL,
    endpoint           TEXT        NOT NULL,
    request_hash       TEXT        NOT NULL,
    response_code      INT         NOT NULL,
    response_body      BYTEA       NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE rights_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE rights_idempotency_keys FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'rights_idempotency_keys' AND policyname = 'tenant_isolation_policy'
    ) THEN
        CREATE POLICY tenant_isolation_policy ON rights_idempotency_keys
            FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
    END IF;
END $$;