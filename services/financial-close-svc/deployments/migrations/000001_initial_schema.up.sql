-- Initial schema for Financial Close Service (financial-close-svc)

CREATE TABLE IF NOT EXISTS fiscal_periods (
    fiscal_period_id    UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    legal_entity_id    VARCHAR(255) NOT NULL,
    period_name        VARCHAR(50) NOT NULL,
    period_start       TIMESTAMP WITH TIME ZONE NOT NULL,
    period_end         TIMESTAMP WITH TIME ZONE NOT NULL,
    close_status       VARCHAR(50) NOT NULL, -- 'OPEN', 'CLOSED', 'LOCKED'
    close_locked_at    TIMESTAMP WITH TIME ZONE,
    evidence_document_id TEXT,
    UNIQUE (tenant_id, legal_entity_id, period_name)
);

CREATE TABLE IF NOT EXISTS close_evidences (
    evidence_id        UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    fiscal_period_id   UUID NOT NULL REFERENCES fiscal_periods(fiscal_period_id) ON DELETE CASCADE,
    trial_balance_hash VARCHAR(255) NOT NULL,
    signature          VARCHAR(255) NOT NULL,
    generated_at       TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE fiscal_periods ENABLE ROW LEVEL SECURITY;
ALTER TABLE close_evidences ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_policy ON fiscal_periods FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON close_evidences FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_fiscal_periods_tenant_entity ON fiscal_periods (tenant_id, legal_entity_id);
CREATE INDEX idx_close_evidences_tenant ON close_evidences (tenant_id);
