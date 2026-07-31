-- Initial schema for Vendor Due Diligence Service (vendor-due-diligence-svc)

CREATE TABLE IF NOT EXISTS vendor_dd_checks (
    check_id                  UUID PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    legal_entity_id           VARCHAR(255) NOT NULL,
    counterparty_id           VARCHAR(255) NOT NULL,
    vendor_name               VARCHAR(255) NOT NULL,
    status                    VARCHAR(20) NOT NULL, -- 'STARTED', 'COMPLETED', 'FAILED'
    risk_outcome              VARCHAR(20), -- 'CLEAR', 'FLAGGED' — null until completed
    screening_basis           TEXT,
    correlation_id            VARCHAR(255) NOT NULL,
    initiated_by_principal_id VARCHAR(255) NOT NULL,
    started_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    completed_at              TIMESTAMP WITH TIME ZONE
);

CREATE TABLE IF NOT EXISTS vendor_dd_evidence (
    evidence_id         UUID PRIMARY KEY,
    check_id            UUID NOT NULL REFERENCES vendor_dd_checks(check_id) ON DELETE CASCADE,
    tenant_id           VARCHAR(255) NOT NULL,
    evidence_type       VARCHAR(100) NOT NULL,
    description         TEXT NOT NULL,
    document_reference  VARCHAR(255),
    recorded_at         TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE vendor_dd_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE vendor_dd_evidence ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_policy ON vendor_dd_checks FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON vendor_dd_evidence FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried "start a check" request with the same
-- (tenant_id, correlation_id) must resolve to the original check, never a
-- second, divergent one.
CREATE UNIQUE INDEX idx_vendor_dd_checks_tenant_correlation ON vendor_dd_checks (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_vendor_dd_checks_tenant_entity ON vendor_dd_checks (tenant_id, legal_entity_id);
CREATE INDEX idx_vendor_dd_checks_tenant_counterparty ON vendor_dd_checks (tenant_id, counterparty_id);
CREATE INDEX idx_vendor_dd_evidence_check ON vendor_dd_evidence (check_id);
