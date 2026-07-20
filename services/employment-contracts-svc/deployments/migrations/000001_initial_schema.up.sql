-- Initial schema for Employment Contracts Service (employment-contracts-svc)

CREATE TABLE IF NOT EXISTS employment_contracts (
    contract_id        UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    legal_entity_id    VARCHAR(255) NOT NULL,
    employee_id        UUID NOT NULL,
    contract_number    VARCHAR(100) NOT NULL,
    version            INT NOT NULL DEFAULT 1,
    contract_type      VARCHAR(50) NOT NULL, -- 'FULL_TIME', 'PART_TIME', 'FIXED_TERM', 'EXECUTIVE'
    status             VARCHAR(50) NOT NULL, -- 'DRAFT', 'ACTIVE', 'SUPERSEDED', 'TERMINATED', 'EXPIRED'
    title              VARCHAR(150) NOT NULL,
    base_salary_amount NUMERIC(18, 4) NOT NULL,
    currency           VARCHAR(3) NOT NULL,
    pay_frequency      VARCHAR(50) NOT NULL, -- 'MONTHLY', 'BIWEEKLY', 'WEEKLY'
    effective_from     DATE NOT NULL,
    effective_to       DATE,
    document_vault_ref VARCHAR(255),
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at         TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS contract_amendments (
    amendment_id     UUID PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL,
    contract_id      UUID NOT NULL REFERENCES employment_contracts(contract_id),
    from_version     INT NOT NULL,
    to_version       INT NOT NULL,
    amendment_reason TEXT NOT NULL,
    amended_by       VARCHAR(255) NOT NULL,
    effective_from   DATE NOT NULL,
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE employment_contracts ENABLE ROW LEVEL SECURITY;
ALTER TABLE contract_amendments ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_contracts ON employment_contracts FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_amendments ON contract_amendments FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance & Uniqueness Indexes
CREATE INDEX idx_contracts_tenant_emp ON employment_contracts (tenant_id, employee_id);
CREATE INDEX idx_contracts_tenant_number ON employment_contracts (tenant_id, contract_number);
CREATE INDEX idx_contracts_tenant_status ON employment_contracts (tenant_id, status);
CREATE INDEX idx_amendments_tenant_contract ON contract_amendments (tenant_id, contract_id);