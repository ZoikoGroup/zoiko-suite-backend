-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS corporate_tax_returns (
    return_id                TEXT          NOT NULL,
    tenant_id                TEXT          NOT NULL,
    legal_entity_id          TEXT          NOT NULL,
    jurisdiction_id          TEXT          NOT NULL,
    tax_registration_number  TEXT          NOT NULL,
    fiscal_year              INTEGER       NOT NULL,
    accounting_period_start  DATE          NOT NULL,
    accounting_period_end    DATE          NOT NULL,
    gross_revenue            NUMERIC(24,4) NOT NULL DEFAULT 0,
    allowable_deductions     NUMERIC(24,4) NOT NULL DEFAULT 0,
    taxable_income           NUMERIC(24,4) NOT NULL DEFAULT 0,
    tax_rate_percent         NUMERIC(8,4)  NOT NULL DEFAULT 0,
    gross_tax_liability      NUMERIC(24,4) NOT NULL DEFAULT 0,
    tax_credits              NUMERIC(24,4) NOT NULL DEFAULT 0,
    net_tax_payable          NUMERIC(24,4) NOT NULL DEFAULT 0,
    tax_already_paid         NUMERIC(24,4) NOT NULL DEFAULT 0,
    balance_due              NUMERIC(24,4) NOT NULL DEFAULT 0,
    currency                 TEXT          NOT NULL DEFAULT 'USD',
    status                   TEXT          NOT NULL DEFAULT 'DRAFT',
    submitted_at             TIMESTAMPTZ,
    submitted_by             TEXT,
    assessed_tax_amount      NUMERIC(24,4),
    assessment_reference     TEXT,
    notes                    TEXT          NOT NULL DEFAULT '',
    effective_from           DATE          NOT NULL,
    effective_to             DATE,
    created_by               TEXT          NOT NULL,
    created_at               TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (return_id, tenant_id),
    -- One return per entity per jurisdiction per fiscal year (unique business key)
    CONSTRAINT uk_corp_tax_return UNIQUE (tenant_id, legal_entity_id, jurisdiction_id, fiscal_year)
);

-- Row-Level Security: all queries must carry app.tenant_id
ALTER TABLE corporate_tax_returns ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS corp_tax_tenant_isolation ON corporate_tax_returns;
CREATE POLICY corp_tax_tenant_isolation ON corporate_tax_returns
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_corp_tax_tenant_entity  ON corporate_tax_returns (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_corp_tax_jurisdiction   ON corporate_tax_returns (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_corp_tax_status         ON corporate_tax_returns (status);
CREATE INDEX IF NOT EXISTS idx_corp_tax_fiscal_year    ON corporate_tax_returns (fiscal_year);

COMMIT;
