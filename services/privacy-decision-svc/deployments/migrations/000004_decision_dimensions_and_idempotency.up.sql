-- 000004_decision_dimensions_and_idempotency.up.sql
-- privacy-decision-svc (PRV-03):
-- 1. Extend privacy_decisions with §12.1 input dimensions & §13.2 durability fields:
--    input_fingerprint, notice_version_id, constraints, subject_context, data_context,
--    secondary_purpose_id, recipient_context, transfer_decision_id.
-- 2. Idempotency storage (§18.1) with tenant isolation & RLS.

-- 1. Extend privacy_decisions
ALTER TABLE privacy_decisions
    ADD COLUMN IF NOT EXISTS input_fingerprint   TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS notice_version_id    TEXT,
    ADD COLUMN IF NOT EXISTS constraints          JSONB       NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS subject_context      JSONB       NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS data_context         JSONB       NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS secondary_purpose_id TEXT,
    ADD COLUMN IF NOT EXISTS recipient_context    JSONB       NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS transfer_decision_id TEXT;

-- 2. Idempotency storage table
CREATE TABLE IF NOT EXISTS decision_idempotency_keys (
    idempotency_key    TEXT        NOT NULL,
    tenant_id          TEXT        NOT NULL,
    endpoint           TEXT        NOT NULL,
    request_hash       TEXT        NOT NULL,
    response_code      INT         NOT NULL,
    response_body      BYTEA       NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE decision_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE decision_idempotency_keys FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'decision_idempotency_keys' AND policyname = 'tenant_isolation_policy'
    ) THEN
        CREATE POLICY tenant_isolation_policy ON decision_idempotency_keys
            FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
    END IF;
END $$;
