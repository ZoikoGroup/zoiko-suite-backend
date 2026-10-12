-- Migration: 000008_add_idempotency_keys.up.sql
--
-- Idempotency key tracking per the API standard (RFC 9457, GCP §16
-- IDEMPOTENCY_MISMATCH). The envelope middleware requires the header, but
-- nothing validated that the same key wasn't reused with a different body.
-- This table records the SHA-256 hash of the request body alongside the
-- key so a mismatch can be detected and refused.

CREATE TABLE idempotency_keys (
    idempotency_key  VARCHAR(256) NOT NULL,
    tenant_id        VARCHAR(64)  NOT NULL,
    body_hash        BYTEA        NOT NULL,
    decision_id      VARCHAR(64)  NOT NULL REFERENCES governance_decisions(decision_id),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

CREATE INDEX idx_idempotency_keys_decision
    ON idempotency_keys (decision_id);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;

ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON idempotency_keys
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));