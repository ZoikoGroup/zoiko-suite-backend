-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS withholding_tax_obligations (
    obligation_id            TEXT          NOT NULL,
    tenant_id                TEXT          NOT NULL,
    legal_entity_id          TEXT          NOT NULL,
    jurisdiction_id          TEXT          NOT NULL,
    counterparty_id          TEXT          NOT NULL,
    payment_reference        TEXT          NOT NULL,
    payment_type             TEXT          NOT NULL DEFAULT 'SERVICES',
    gross_payment_amount     NUMERIC(24,4) NOT NULL DEFAULT 0,
    taxable_base_amount      NUMERIC(24,4) NOT NULL DEFAULT 0,
    withholding_rate_percent NUMERIC(8,4)  NOT NULL DEFAULT 0,
    withheld_amount          NUMERIC(24,4) NOT NULL DEFAULT 0,
    currency                 TEXT          NOT NULL DEFAULT 'USD',
    tax_rule_id             TEXT          NOT NULL DEFAULT '',
    tax_treaty_exemption     BOOLEAN       NOT NULL DEFAULT false,
    exemption_certificate_ref TEXT          NOT NULL DEFAULT '',
    status                   TEXT          NOT NULL DEFAULT 'DRAFT',
    remittance_reference     TEXT,
    remitted_at              TIMESTAMPTZ,
    remitted_by              TEXT,
    notes                    TEXT          NOT NULL DEFAULT '',
    effective_from           DATE          NOT NULL,
    effective_to             DATE,
    created_by               TEXT          NOT NULL,
    created_at               TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (obligation_id, tenant_id)
);

-- Row-Level Security: all queries must carry app.tenant_id
ALTER TABLE withholding_tax_obligations ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS wht_obligations_tenant_isolation ON withholding_tax_obligations;
CREATE POLICY wht_obligations_tenant_isolation ON withholding_tax_obligations
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_wht_tenant_entity   ON withholding_tax_obligations (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_wht_jurisdiction    ON withholding_tax_obligations (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_wht_counterparty    ON withholding_tax_obligations (counterparty_id);
CREATE INDEX IF NOT EXISTS idx_wht_status          ON withholding_tax_obligations (status);
CREATE INDEX IF NOT EXISTS idx_wht_payment_ref     ON withholding_tax_obligations (payment_reference);

COMMIT;
