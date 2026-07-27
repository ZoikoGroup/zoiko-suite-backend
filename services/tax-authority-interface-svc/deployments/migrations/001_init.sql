CREATE TABLE IF NOT EXISTS tax_interfaces (
    interface_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    jurisdiction VARCHAR(64) NOT NULL,
    authority_name VARCHAR(255) NOT NULL,
    protocol VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tax_interfaces_tenant_entity ON tax_interfaces(tenant_id, legal_entity_id);

CREATE TABLE IF NOT EXISTS tax_filing_submissions (
    submission_id VARCHAR(64) PRIMARY KEY,
    interface_id VARCHAR(64) NOT NULL REFERENCES tax_interfaces(interface_id) ON DELETE CASCADE,
    tenant_id VARCHAR(64) NOT NULL,
    tax_period VARCHAR(32) NOT NULL,
    filing_type VARCHAR(64) NOT NULL,
    tax_amount NUMERIC(18, 4) NOT NULL DEFAULT 0.0,
    status VARCHAR(32) NOT NULL DEFAULT 'SUBMITTED',
    ack_reference VARCHAR(128),
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tax_filings_interface ON tax_filing_submissions(interface_id, submitted_at);
