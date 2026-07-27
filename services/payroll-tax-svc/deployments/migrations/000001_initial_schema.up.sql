-- Initial schema for Payroll Tax Service (payroll-tax-svc)

CREATE TABLE IF NOT EXISTS tax_jurisdiction_profiles (
    profile_id          UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    legal_entity_id     VARCHAR(255) NOT NULL,
    jurisdiction_code   VARCHAR(100) NOT NULL,
    tax_engine_type     VARCHAR(50) NOT NULL, -- 'STANDARD_ENGINE', 'LOCAL_PROVIDER', 'EXTERNAL_SERVICE', 'GOVERNMENT_DIRECT'
    provider_endpoint   VARCHAR(255),
    status              VARCHAR(50) NOT NULL, -- 'ACTIVE', 'INACTIVE'
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS tax_calculation_records (
    calculation_id           UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    payroll_run_id           UUID NOT NULL,
    employee_id              UUID NOT NULL,
    jurisdiction_code        VARCHAR(100) NOT NULL,
    gross_taxable_amount     NUMERIC(18, 4) NOT NULL,
    pre_tax_deduction_amount NUMERIC(18, 4) NOT NULL DEFAULT 0.00,
    taxable_basis            NUMERIC(18, 4) NOT NULL,
    total_tax_amount         NUMERIC(18, 4) NOT NULL,
    tax_breakdown            JSONB NOT NULL,
    engine_type              VARCHAR(50) NOT NULL,
    rule_version_used        VARCHAR(50) NOT NULL,
    status                   VARCHAR(50) NOT NULL, -- 'CALCULATED', 'AUDITED', 'ADJUSTED'
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS tax_basis_audits (
    audit_id               UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    calculation_id         UUID NOT NULL REFERENCES tax_calculation_records(calculation_id),
    employee_id            UUID NOT NULL,
    rule_basis_json        JSONB NOT NULL,
    provider_metadata_json JSONB NOT NULL,
    audited_at             TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE tax_jurisdiction_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_calculation_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_basis_audits ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_tax_profiles ON tax_jurisdiction_profiles FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_tax_calcs ON tax_calculation_records FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_tax_audits ON tax_basis_audits FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_tax_profiles_tenant_entity ON tax_jurisdiction_profiles (tenant_id, legal_entity_id);
CREATE INDEX idx_tax_profiles_tenant_jurisdiction ON tax_jurisdiction_profiles (tenant_id, jurisdiction_code);
CREATE INDEX idx_tax_calcs_tenant_run ON tax_calculation_records (tenant_id, payroll_run_id);
CREATE INDEX idx_tax_calcs_tenant_emp ON tax_calculation_records (tenant_id, employee_id);
CREATE INDEX idx_tax_audits_tenant_calc ON tax_basis_audits (tenant_id, calculation_id);