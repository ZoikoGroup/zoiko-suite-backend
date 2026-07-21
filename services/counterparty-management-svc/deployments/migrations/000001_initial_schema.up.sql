-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS counterparties (
    counterparty_id     TEXT        NOT NULL,
    tenant_id           TEXT        NOT NULL,
    legal_entity_id     TEXT        NOT NULL,
    name                TEXT        NOT NULL,
    counterparty_type   TEXT        NOT NULL,
    registration_number TEXT,
    tax_id              TEXT,
    jurisdiction_id     TEXT        NOT NULL,
    risk_category       TEXT        NOT NULL DEFAULT 'MEDIUM',
    status              TEXT        NOT NULL DEFAULT 'ONBOARDING',
    contact_email       TEXT,
    phone               TEXT,
    address             TEXT,
    compliance_status   TEXT        NOT NULL DEFAULT 'PENDING',
    effective_from      DATE        NOT NULL,
    effective_to        DATE,
    created_by          TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (counterparty_id, tenant_id)
);

-- Row-Level Security
ALTER TABLE counterparties ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS counterparties_tenant_isolation ON counterparties;
CREATE POLICY counterparties_tenant_isolation ON counterparties
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_counterparties_tenant_entity ON counterparties (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_counterparties_type ON counterparties (counterparty_type);
CREATE INDEX IF NOT EXISTS idx_counterparties_status ON counterparties (status);
CREATE INDEX IF NOT EXISTS idx_counterparties_jurisdiction ON counterparties (jurisdiction_id);

COMMIT;
