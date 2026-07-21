-- Initial schema for Benefits Service (benefits-svc)

CREATE TABLE IF NOT EXISTS benefit_plans (
    plan_id                      UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id              VARCHAR(255) NOT NULL,
    name                         VARCHAR(150) NOT NULL,
    plan_type                    VARCHAR(50) NOT NULL, -- 'HEALTH_INSURANCE', 'DENTAL', 'VISION', 'RETIREMENT_401K', 'HSA', 'LIFE_INSURANCE'
    provider_name                VARCHAR(150) NOT NULL,
    deduction_tax_treatment      VARCHAR(50) NOT NULL, -- 'PRE_TAX', 'POST_TAX'
    employer_contribution_pct    NUMERIC(5, 2) NOT NULL DEFAULT 0.00,
    employee_contribution_amount NUMERIC(18, 4) NOT NULL DEFAULT 0.00,
    currency                     VARCHAR(3) NOT NULL,
    status                       VARCHAR(50) NOT NULL, -- 'ACTIVE', 'INACTIVE'
    created_at                   TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                   TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS benefit_elections (
    election_id                  UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    employee_id                  UUID NOT NULL,
    plan_id                      UUID NOT NULL REFERENCES benefit_plans(plan_id),
    coverage_level               VARCHAR(50) NOT NULL, -- 'EMPLOYEE_ONLY', 'EMPLOYEE_PLUS_SPOUSE', 'EMPLOYEE_PLUS_FAMILY'
    employee_contribution_amount NUMERIC(18, 4) NOT NULL,
    employer_contribution_amount NUMERIC(18, 4) NOT NULL,
    effective_from               DATE NOT NULL,
    effective_to                 DATE,
    status                       VARCHAR(50) NOT NULL, -- 'ACTIVE', 'CANCELLED'
    created_at                   TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at                   TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE benefit_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE benefit_elections ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_benefit_plans ON benefit_plans FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_benefit_elections ON benefit_elections FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance & Uniqueness Indexes
CREATE INDEX idx_benefit_plans_tenant_entity ON benefit_plans (tenant_id, legal_entity_id);
CREATE INDEX idx_benefit_plans_tenant_status ON benefit_plans (tenant_id, status);
CREATE INDEX idx_benefit_elections_tenant_emp ON benefit_elections (tenant_id, employee_id);
CREATE INDEX idx_benefit_elections_tenant_status ON benefit_elections (tenant_id, status);