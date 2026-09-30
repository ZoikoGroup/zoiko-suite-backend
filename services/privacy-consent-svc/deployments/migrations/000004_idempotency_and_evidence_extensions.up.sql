-- 000004_idempotency_and_evidence_extensions.up.sql
-- privacy-consent-svc: Idempotency-Key support (§18.1),
-- Proxy / Authorized Representative evidence (§11.1),
-- Affirmative-action evidence (§10.1),
-- Presentation receipt extended fields (§10.1, §18).

-- 1. Extend consent_receipts with proxy and affirmative action evidence
ALTER TABLE consent_receipts
    ADD COLUMN IF NOT EXISTS is_proxy BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS representative_subject_ref TEXT,
    ADD COLUMN IF NOT EXISTS representative_authority_ref TEXT,
    ADD COLUMN IF NOT EXISTS representative_evidence TEXT,
    ADD COLUMN IF NOT EXISTS affirmative_action_type VARCHAR(64) NOT NULL DEFAULT 'EXPLICIT_CHECKBOX',
    ADD COLUMN IF NOT EXISTS affirmative_evidence TEXT;

-- 2. Extend presentation_receipts with session, template and delivery evidence
ALTER TABLE presentation_receipts
    ADD COLUMN IF NOT EXISTS session_ref TEXT,
    ADD COLUMN IF NOT EXISTS template_version VARCHAR(64),
    ADD COLUMN IF NOT EXISTS delivery_evidence TEXT;

-- 3. Idempotency storage with tenant isolation
CREATE TABLE IF NOT EXISTS consent_idempotency_keys (
    idempotency_key    TEXT        NOT NULL,
    tenant_id          TEXT        NOT NULL,
    endpoint           TEXT        NOT NULL,
    request_hash       TEXT        NOT NULL,
    response_code      INT         NOT NULL,
    response_body      BYTEA       NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE consent_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_idempotency_keys FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'consent_idempotency_keys' AND policyname = 'tenant_isolation_policy'
    ) THEN
        CREATE POLICY tenant_isolation_policy ON consent_idempotency_keys
            FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
    END IF;
END $$;
