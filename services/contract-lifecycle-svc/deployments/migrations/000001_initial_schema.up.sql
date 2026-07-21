-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS contracts (
    contract_id       TEXT        NOT NULL,
    tenant_id         TEXT        NOT NULL,
    legal_entity_id   TEXT        NOT NULL,
    contract_type     TEXT        NOT NULL,
    title             TEXT        NOT NULL,
    description       TEXT,
    counterparty_id   TEXT        NOT NULL,
    counterparty_name TEXT        NOT NULL,
    status            TEXT        NOT NULL DEFAULT 'DRAFT',
    version           INTEGER     NOT NULL DEFAULT 1,
    effective_from    DATE        NOT NULL,
    effective_to      DATE,
    signed_at         TIMESTAMPTZ,
    signed_by         TEXT,
    terminated_at     TIMESTAMPTZ,
    terminated_by     TEXT,
    termination_note  TEXT,
    currency          TEXT        NOT NULL DEFAULT 'USD',
    total_value       NUMERIC(20,4) NOT NULL DEFAULT 0,
    document_vault_id TEXT,
    created_by        TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (contract_id, tenant_id)
);

CREATE TABLE IF NOT EXISTS contract_versions (
    version_id      TEXT        NOT NULL PRIMARY KEY,
    contract_id     TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    version_number  INTEGER     NOT NULL,
    status          TEXT        NOT NULL,
    title           TEXT        NOT NULL,
    description     TEXT,
    effective_from  DATE        NOT NULL,
    effective_to    DATE,
    change_summary  TEXT        NOT NULL DEFAULT '',
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Row-Level Security
ALTER TABLE contracts ENABLE ROW LEVEL SECURITY;
ALTER TABLE contract_versions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS contracts_tenant_isolation ON contracts;
CREATE POLICY contracts_tenant_isolation ON contracts
    USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS contract_versions_tenant_isolation ON contract_versions;
CREATE POLICY contract_versions_tenant_isolation ON contract_versions
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_contracts_tenant_legal_entity   ON contracts (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_contracts_counterparty           ON contracts (counterparty_id);
CREATE INDEX IF NOT EXISTS idx_contracts_status                 ON contracts (status);
CREATE INDEX IF NOT EXISTS idx_contract_versions_contract_id   ON contract_versions (contract_id);

COMMIT;
