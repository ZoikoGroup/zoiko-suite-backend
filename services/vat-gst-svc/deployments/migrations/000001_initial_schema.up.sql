-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS vat_returns (
    return_id               TEXT          NOT NULL,
    tenant_id               TEXT          NOT NULL,
    legal_entity_id         TEXT          NOT NULL,
    jurisdiction_id         TEXT          NOT NULL,
    tax_registration_number TEXT          NOT NULL,
    tax_period              TEXT          NOT NULL,
    total_sales_amount      NUMERIC(20,4) NOT NULL DEFAULT 0,
    total_purchase_amount   NUMERIC(20,4) NOT NULL DEFAULT 0,
    output_tax_amount       NUMERIC(20,4) NOT NULL DEFAULT 0,
    input_tax_amount        NUMERIC(20,4) NOT NULL DEFAULT 0,
    net_tax_payable         NUMERIC(20,4) NOT NULL DEFAULT 0,
    currency                TEXT          NOT NULL DEFAULT 'USD',
    status                  TEXT          NOT NULL DEFAULT 'DRAFT',
    filed_at                TIMESTAMPTZ,
    filed_by                TEXT,
    effective_from          DATE          NOT NULL,
    effective_to            DATE,
    created_by              TEXT          NOT NULL,
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (return_id, tenant_id),
    CONSTRAINT uk_vat_returns_period UNIQUE (tenant_id, legal_entity_id, jurisdiction_id, tax_period)
);

-- Row-Level Security
ALTER TABLE vat_returns ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS vat_returns_tenant_isolation ON vat_returns;
CREATE POLICY vat_returns_tenant_isolation ON vat_returns
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_vat_returns_tenant_entity ON vat_returns (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_vat_returns_jurisdiction ON vat_returns (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_vat_returns_status ON vat_returns (status);
CREATE INDEX IF NOT EXISTS idx_vat_returns_period ON vat_returns (tax_period);

COMMIT;
