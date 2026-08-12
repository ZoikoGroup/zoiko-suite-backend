-- Initial schema for Procurement Workflow Service (procurement-workflow-svc)

CREATE TABLE IF NOT EXISTS procurement_cases (
    case_id                    UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    legal_entity_id            VARCHAR(255) NOT NULL,
    requested_by_principal_id  VARCHAR(255) NOT NULL,
    description                TEXT NOT NULL,
    category                   VARCHAR(100) NOT NULL,
    amount                     NUMERIC(18,4) NOT NULL CHECK (amount > 0),
    currency_code              VARCHAR(10) NOT NULL,
    vendor_profile_id          VARCHAR(255),
    status                     VARCHAR(30) NOT NULL, -- REQUESTED, SPEND_BLOCKED, APPROVAL_PENDING, APPROVED, REJECTED, COMPLETED
    spend_check_decision       VARCHAR(20),           -- ALLOWED, BLOCKED
    spend_check_basis          VARCHAR(50),
    purchase_order_id          VARCHAR(255),
    approved_by_principal_id   VARCHAR(255),
    rejected_by_principal_id   VARCHAR(255),
    rejection_reason           TEXT,
    correlation_id             VARCHAR(255) NOT NULL,
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                 TIMESTAMP WITH TIME ZONE NOT NULL,
    approved_at                TIMESTAMP WITH TIME ZONE,
    rejected_at                TIMESTAMP WITH TIME ZONE,
    completed_at               TIMESTAMP WITH TIME ZONE
);

-- Enable Row-Level Security
ALTER TABLE procurement_cases ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policy
CREATE POLICY tenant_isolation_policy ON procurement_cases FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried case-create with the same (tenant_id, correlation_id)
-- must resolve to the original case record, never a duplicate.
CREATE UNIQUE INDEX idx_procurement_cases_tenant_correlation ON procurement_cases (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_procurement_cases_tenant_entity_status ON procurement_cases (tenant_id, legal_entity_id, status);
