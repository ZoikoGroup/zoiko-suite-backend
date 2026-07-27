-- Initial schema for Payroll Exceptions Service (payroll-exceptions-svc)

CREATE TABLE IF NOT EXISTS payroll_exceptions (
    exception_id     UUID PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL,
    payroll_run_id   UUID NOT NULL,
    employee_id      UUID,
    exception_code   VARCHAR(100) NOT NULL,
    severity         VARCHAR(50) NOT NULL, -- 'BLOCKER', 'CRITICAL', 'WARNING'
    description      TEXT NOT NULL,
    details_json     JSONB NOT NULL DEFAULT '{}'::jsonb,
    status           VARCHAR(50) NOT NULL, -- 'OPEN', 'IN_REVIEW', 'RESOLVED', 'WAIVED'
    resolution_notes TEXT,
    resolved_by      VARCHAR(255),
    resolved_at      TIMESTAMP WITH TIME ZONE,
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE payroll_exceptions ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policy
CREATE POLICY tenant_isolation_exceptions ON payroll_exceptions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_exceptions_tenant_run ON payroll_exceptions (tenant_id, payroll_run_id);
CREATE INDEX idx_exceptions_tenant_emp ON payroll_exceptions (tenant_id, employee_id);
CREATE INDEX idx_exceptions_tenant_status_sev ON payroll_exceptions (tenant_id, status, severity);