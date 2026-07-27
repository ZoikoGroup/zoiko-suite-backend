-- Initial schema for Invoice Approval Service (invoice-approval-svc)

CREATE TABLE IF NOT EXISTS invoice_approval_requests (
    approval_request_id     UUID PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    invoice_id              UUID NOT NULL,
    workflow_instance_id    UUID NOT NULL,
    invoice_amount          NUMERIC(18,4) NOT NULL CHECK (invoice_amount > 0),
    currency_code           VARCHAR(10) NOT NULL,
    status                  VARCHAR(50) NOT NULL, -- 'PENDING', 'APPROVED', 'REJECTED'
    current_step            INT NOT NULL DEFAULT 1,
    total_steps             INT NOT NULL DEFAULT 1,
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at              TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS approval_decisions (
    approval_decision_id    UUID PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    approval_request_id     UUID NOT NULL REFERENCES invoice_approval_requests(approval_request_id) ON DELETE CASCADE,
    step_number             INT NOT NULL,
    decided_by_principal_id VARCHAR(255) NOT NULL,
    decision                VARCHAR(50) NOT NULL, -- 'APPROVED', 'REJECTED'
    decision_reason         TEXT,
    decided_at              TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE invoice_approval_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_decisions ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_policy ON invoice_approval_requests FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON approval_decisions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_invoice_approval_requests_tenant_invoice ON invoice_approval_requests (tenant_id, invoice_id);
CREATE INDEX idx_invoice_approval_requests_tenant_entity ON invoice_approval_requests (tenant_id, legal_entity_id, status);
CREATE INDEX idx_approval_decisions_request ON approval_decisions (approval_request_id);