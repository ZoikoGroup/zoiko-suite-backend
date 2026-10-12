-- Tenant-scoped tables. Unlike the global registry tables, these carry
-- tenant_id and FORCE row-level security.

-- A tenant's overlay on a globally SUPPORTED currency. Never deleted: disabling
-- sets enabled = false (so the history of "was this ever enabled" survives).
CREATE TABLE IF NOT EXISTS tenant_currency_support (
    tenant_id    TEXT NOT NULL,
    currency_id  UUID NOT NULL REFERENCES currencies (currency_id),
    enabled      BOOLEAN NOT NULL,
    version      BIGINT NOT NULL DEFAULT 1,
    reason       TEXT NOT NULL,
    actor        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, currency_id),
    CONSTRAINT tenant_currency_support_version_positive CHECK (version >= 1)
);
ALTER TABLE tenant_currency_support ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_currency_support FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON tenant_currency_support FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE TRIGGER trg_tenant_currency_support_no_delete
    BEFORE DELETE ON tenant_currency_support
    FOR EACH ROW EXECUTE FUNCTION currency_registry_forbid_delete();

-- Idempotency: the material result of a command, keyed by the caller's
-- Idempotency-Key within its tenant. A replay with the same request returns
-- `response` verbatim; the same key with a different request is refused.
CREATE TABLE IF NOT EXISTS currency_idempotency (
    tenant_id        TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL,
    operation        TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    response         JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);
ALTER TABLE currency_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE currency_idempotency FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON currency_idempotency FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
