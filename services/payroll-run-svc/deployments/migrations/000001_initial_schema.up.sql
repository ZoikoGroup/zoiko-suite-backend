-- Initial schema for Payroll Run Service (payroll-run-svc)

CREATE TABLE IF NOT EXISTS payroll_runs (
    run_id                UUID PRIMARY KEY,
    tenant_id             VARCHAR(255) NOT NULL,
    legal_entity_id       VARCHAR(255) NOT NULL,
    run_number            VARCHAR(100) NOT NULL,
    pay_period_start      DATE NOT NULL,
    pay_period_end        DATE NOT NULL,
    pay_date              DATE NOT NULL,
    status                VARCHAR(50) NOT NULL, -- 'INITIATED', 'CALCULATED', 'BLOCKED', 'COMPLETED'
    is_shadow_run         BOOLEAN NOT NULL DEFAULT false,
    total_gross_pay       NUMERIC(18, 4) NOT NULL DEFAULT 0,
    total_net_pay         NUMERIC(18, 4) NOT NULL DEFAULT 0,
    total_tax_deductions  NUMERIC(18, 4) NOT NULL DEFAULT 0,
    total_other_deductions NUMERIC(18, 4) NOT NULL DEFAULT 0,
    employee_count        INT NOT NULL DEFAULT 0,
    created_at            TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at            TIMESTAMP WITH TIME ZONE NOT NULL,
    finalized_at          TIMESTAMP WITH TIME ZONE
);

CREATE TABLE IF NOT EXISTS pay_slips (
    slip_id             UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    run_id              UUID NOT NULL REFERENCES payroll_runs(run_id) ON DELETE CASCADE,
    employee_id         UUID NOT NULL,
    employee_number     VARCHAR(100) NOT NULL,
    employee_name       VARCHAR(200) NOT NULL,
    gross_pay           NUMERIC(18, 4) NOT NULL,
    tax_withheld        NUMERIC(18, 4) NOT NULL,
    benefits_deductions NUMERIC(18, 4) NOT NULL,
    net_pay             NUMERIC(18, 4) NOT NULL,
    currency            VARCHAR(3) NOT NULL,
    effective_date      DATE NOT NULL,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS shadow_payroll_comparisons (
    comparison_id       UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    run_id              UUID NOT NULL REFERENCES payroll_runs(run_id) ON DELETE CASCADE,
    employee_id         UUID NOT NULL,
    legacy_gross_pay    NUMERIC(18, 4) NOT NULL,
    legacy_net_pay      NUMERIC(18, 4) NOT NULL,
    legacy_tax_withheld NUMERIC(18, 4) NOT NULL,
    zoiko_gross_pay     NUMERIC(18, 4) NOT NULL,
    zoiko_net_pay       NUMERIC(18, 4) NOT NULL,
    zoiko_tax_withheld  NUMERIC(18, 4) NOT NULL,
    gross_variance      NUMERIC(18, 4) NOT NULL,
    net_variance        NUMERIC(18, 4) NOT NULL,
    tax_variance        NUMERIC(18, 4) NOT NULL,
    is_equivalent       BOOLEAN NOT NULL,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE payroll_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE pay_slips ENABLE ROW LEVEL SECURITY;
ALTER TABLE shadow_payroll_comparisons ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_payroll_runs ON payroll_runs FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_pay_slips ON pay_slips FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_shadow_comparisons ON shadow_payroll_comparisons FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance & Uniqueness Indexes
CREATE INDEX idx_payroll_runs_tenant_entity ON payroll_runs (tenant_id, legal_entity_id);
CREATE INDEX idx_payroll_runs_tenant_status ON payroll_runs (tenant_id, status);
CREATE INDEX idx_pay_slips_tenant_run ON pay_slips (tenant_id, run_id);
CREATE INDEX idx_pay_slips_tenant_emp ON pay_slips (tenant_id, employee_id);
CREATE INDEX idx_shadow_comp_tenant_run ON shadow_payroll_comparisons (tenant_id, run_id);