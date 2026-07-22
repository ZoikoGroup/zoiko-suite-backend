-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS tax_determinations (
    determination_id      TEXT          NOT NULL,
    tenant_id             TEXT          NOT NULL,
    transaction_id        TEXT          NOT NULL,
    source_module         TEXT          NOT NULL,
    legal_entity_id       TEXT          NOT NULL,
    jurisdiction_id       TEXT          NOT NULL,
    rule_id               TEXT,
    tax_category          TEXT          NOT NULL,
    gross_amount          NUMERIC(20,4) NOT NULL DEFAULT 0,
    taxable_amount        NUMERIC(20,4) NOT NULL DEFAULT 0,
    tax_rate_percentage  NUMERIC(7,4)  NOT NULL DEFAULT 0,
    calculated_tax_amount NUMERIC(20,4) NOT NULL DEFAULT 0,
    exempt_amount         NUMERIC(20,4) NOT NULL DEFAULT 0,
    currency              TEXT          NOT NULL DEFAULT 'USD',
    status                TEXT          NOT NULL DEFAULT 'CALCULATED',
    effective_from        DATE          NOT NULL,
    effective_to          DATE,
    evaluated_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    evaluated_by          TEXT          NOT NULL,
    created_at            TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (determination_id, tenant_id)
);

-- Row-Level Security
ALTER TABLE tax_determinations ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tax_determinations_tenant_isolation ON tax_determinations;
CREATE POLICY tax_determinations_tenant_isolation ON tax_determinations
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_tax_determinations_tenant_trans ON tax_determinations (tenant_id, transaction_id);
CREATE INDEX IF NOT EXISTS idx_tax_determinations_jurisdiction ON tax_determinations (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_tax_determinations_status ON tax_determinations (status);
CREATE INDEX IF NOT EXISTS idx_tax_determinations_rule ON tax_determinations (rule_id);

COMMIT;
