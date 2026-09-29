-- 000004_idempotency_and_measures.up.sql
-- privacy-transfer-svc (PRV-05):
-- 1. Add structured assessment measures columns (government_access_risk, technical_measures, organizational_measures) per §16.1.
-- 2. Idempotency storage (§18.1) with tenant isolation & RLS.

-- 1. Add structured assessment measures columns
ALTER TABLE transfer_assessments
    ADD COLUMN IF NOT EXISTS government_access_risk TEXT,
    ADD COLUMN IF NOT EXISTS technical_measures TEXT,
    ADD COLUMN IF NOT EXISTS organizational_measures TEXT;

-- 2. Idempotency storage table
CREATE TABLE IF NOT EXISTS transfer_idempotency_keys (
    idempotency_key    TEXT        NOT NULL,
    tenant_id          TEXT        NOT NULL,
    endpoint           TEXT        NOT NULL,
    request_hash       TEXT        NOT NULL,
    response_code      INT         NOT NULL,
    response_body      BYTEA       NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE transfer_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE transfer_idempotency_keys FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'transfer_idempotency_keys' AND policyname = 'tenant_isolation_policy'
    ) THEN
        CREATE POLICY tenant_isolation_policy ON transfer_idempotency_keys
            FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
    END IF;
END $$;
