-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS tax_rules (
    rule_id             TEXT          NOT NULL,
    tenant_id           TEXT          NOT NULL,
    jurisdiction_id     TEXT          NOT NULL,
    rule_code           TEXT          NOT NULL,
    name                TEXT          NOT NULL,
    category            TEXT          NOT NULL,
    tax_rate_percentage NUMERIC(7,4)  NOT NULL DEFAULT 0,
    standard_deductions NUMERIC(20,4) NOT NULL DEFAULT 0,
    exemptions_json     TEXT          NOT NULL DEFAULT '{}',
    status              TEXT          NOT NULL DEFAULT 'DRAFT',
    version             INTEGER       NOT NULL DEFAULT 1,
    effective_from      DATE          NOT NULL,
    effective_to        DATE,
    created_by          TEXT          NOT NULL,
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, tenant_id),
    CONSTRAINT uk_tax_rules_code UNIQUE (tenant_id, jurisdiction_id, rule_code, version)
);

-- Row-Level Security
ALTER TABLE tax_rules ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tax_rules_tenant_isolation ON tax_rules;
CREATE POLICY tax_rules_tenant_isolation ON tax_rules
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_tax_rules_jurisdiction ON tax_rules (tenant_id, jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_tax_rules_category ON tax_rules (category);
CREATE INDEX IF NOT EXISTS idx_tax_rules_code ON tax_rules (rule_code);

COMMIT;
