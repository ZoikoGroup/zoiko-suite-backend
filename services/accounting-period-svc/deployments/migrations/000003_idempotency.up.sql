-- Idempotency: the material result of a command, keyed by the caller's
-- Idempotency-Key within its tenant. A replay with the same request returns
-- `response` verbatim; the same key with a different request is refused.
CREATE TABLE IF NOT EXISTS accounting_period_idempotency (
    tenant_id        TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL,
    operation        TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    response         JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);
ALTER TABLE accounting_period_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_period_idempotency FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accounting_period_idempotency FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
