-- Initial schema for Compensation Service (compensation-svc)

CREATE TABLE IF NOT EXISTS compensation_structures (
    structure_id        UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    legal_entity_id     VARCHAR(255) NOT NULL,
    name                VARCHAR(150) NOT NULL,
    pay_type            VARCHAR(50) NOT NULL, -- 'SALARY', 'HOURLY'
    min_amount          NUMERIC(18, 4) NOT NULL,
    max_amount          NUMERIC(18, 4) NOT NULL,
    currency            VARCHAR(3) NOT NULL,
    overtime_multiplier NUMERIC(5, 2) NOT NULL DEFAULT 1.50,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS wage_revisions (
    revision_id     UUID PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    employee_id     UUID NOT NULL,
    structure_id    UUID REFERENCES compensation_structures(structure_id),
    pay_type        VARCHAR(50) NOT NULL, -- 'SALARY', 'HOURLY'
    amount          NUMERIC(18, 4) NOT NULL,
    currency        VARCHAR(3) NOT NULL,
    effective_from  DATE NOT NULL,
    effective_to    DATE,
    reason          TEXT NOT NULL,
    revised_by      VARCHAR(255) NOT NULL,
    status          VARCHAR(50) NOT NULL, -- 'ACTIVE', 'SUPERSEDED'
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS bonus_grants (
    grant_id    UUID PRIMARY KEY,
    tenant_id   VARCHAR(255) NOT NULL,
    employee_id UUID NOT NULL,
    bonus_type  VARCHAR(50) NOT NULL, -- 'PERFORMANCE', 'ANNUAL', 'SIGNING', 'RETENTION'
    amount      NUMERIC(18, 4) NOT NULL,
    currency    VARCHAR(3) NOT NULL,
    grant_date  DATE NOT NULL,
    status      VARCHAR(50) NOT NULL, -- 'PENDING', 'APPROVED', 'PAID', 'CANCELLED'
    approved_by VARCHAR(255),
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE compensation_structures ENABLE ROW LEVEL SECURITY;
ALTER TABLE wage_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_grants ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_comp_structures ON compensation_structures FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_wage_revisions ON wage_revisions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_bonus_grants ON bonus_grants FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance & Uniqueness Indexes
CREATE INDEX idx_comp_struct_tenant_entity ON compensation_structures (tenant_id, legal_entity_id);
CREATE INDEX idx_wage_rev_tenant_emp ON wage_revisions (tenant_id, employee_id);
CREATE INDEX idx_wage_rev_tenant_status ON wage_revisions (tenant_id, status);
CREATE INDEX idx_bonus_grants_tenant_emp ON bonus_grants (tenant_id, employee_id);
CREATE INDEX idx_bonus_grants_tenant_status ON bonus_grants (tenant_id, status);