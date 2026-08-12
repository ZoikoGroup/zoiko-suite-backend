-- Initial schema for Performance & Review Service (performance-review-svc)

CREATE TABLE IF NOT EXISTS review_cycles (
    cycle_id                   UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    legal_entity_id            VARCHAR(255) NOT NULL,
    cycle_name                 VARCHAR(255) NOT NULL,
    period_start                DATE NOT NULL,
    period_end                  DATE NOT NULL,
    status                     VARCHAR(20) NOT NULL, -- OPEN, CLOSED
    created_by_principal_id    VARCHAR(255) NOT NULL,
    correlation_id             VARCHAR(255) NOT NULL,
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                 TIMESTAMP WITH TIME ZONE NOT NULL,
    closed_at                  TIMESTAMP WITH TIME ZONE
);

CREATE TABLE IF NOT EXISTS review_records (
    review_id                  UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    legal_entity_id            VARCHAR(255) NOT NULL,
    cycle_id                   UUID NOT NULL REFERENCES review_cycles(cycle_id) ON DELETE CASCADE,
    employee_id                VARCHAR(255) NOT NULL,
    reviewer_principal_id      VARCHAR(255) NOT NULL,
    rating                     INTEGER CHECK (rating IS NULL OR (rating >= 1 AND rating <= 5)),
    comments                   TEXT,
    status                     VARCHAR(20) NOT NULL, -- DRAFT, SUBMITTED, COMPLETED
    created_by_principal_id    VARCHAR(255) NOT NULL,
    correlation_id             VARCHAR(255) NOT NULL,
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                 TIMESTAMP WITH TIME ZONE NOT NULL,
    submitted_at               TIMESTAMP WITH TIME ZONE,
    completed_at                TIMESTAMP WITH TIME ZONE
);

-- Enable Row-Level Security
ALTER TABLE review_cycles ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_records ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_policy ON review_cycles FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON review_records FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried create with the same (tenant_id, correlation_id)
-- must resolve to the original record, never a duplicate.
CREATE UNIQUE INDEX idx_review_cycles_tenant_correlation ON review_cycles (tenant_id, correlation_id);
CREATE UNIQUE INDEX idx_review_records_tenant_correlation ON review_records (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_review_cycles_tenant_entity_status ON review_cycles (tenant_id, legal_entity_id, status);
CREATE INDEX idx_review_records_tenant_cycle ON review_records (tenant_id, cycle_id);
CREATE INDEX idx_review_records_tenant_employee ON review_records (tenant_id, employee_id);
