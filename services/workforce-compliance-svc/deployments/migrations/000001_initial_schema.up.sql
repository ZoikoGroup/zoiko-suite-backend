-- 000001_initial_schema.up.sql: Workforce Compliance Service

CREATE TABLE IF NOT EXISTS work_authorizations (
    auth_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    employee_id VARCHAR(64) NOT NULL,
    document_type VARCHAR(64) NOT NULL,
    document_number VARCHAR(128) NOT NULL,
    issue_date DATE NOT NULL,
    expiry_date DATE,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    verified_by VARCHAR(64),
    verified_at TIMESTAMPTZ,
    effective_from DATE NOT NULL,
    effective_to DATE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_work_auth_tenant_emp ON work_authorizations(tenant_id, employee_id);

ALTER TABLE work_authorizations ENABLE ROW LEVEL SECURITY;

CREATE POLICY work_authorizations_tenant_isolation ON work_authorizations
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Visa Records
CREATE TABLE IF NOT EXISTS visa_records (
    visa_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    employee_id VARCHAR(64) NOT NULL,
    visa_type VARCHAR(64) NOT NULL,
    issuing_country VARCHAR(3) NOT NULL,
    expiration_date DATE NOT NULL,
    grace_period_days INT NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL DEFAULT 'VERIFIED',
    flagged_for_expiry BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_visa_records_tenant_emp ON visa_records(tenant_id, employee_id);

ALTER TABLE visa_records ENABLE ROW LEVEL SECURITY;

CREATE POLICY visa_records_tenant_isolation ON visa_records
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Working Hour Logs
CREATE TABLE IF NOT EXISTS working_hour_logs (
    log_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    employee_id VARCHAR(64) NOT NULL,
    work_date DATE NOT NULL,
    hours_worked NUMERIC(5,2) NOT NULL,
    overtime_hours NUMERIC(5,2) NOT NULL DEFAULT 0,
    weekly_accumulated NUMERIC(6,2) NOT NULL DEFAULT 0,
    is_breached BOOLEAN NOT NULL DEFAULT FALSE,
    max_allowed_hours NUMERIC(5,2) NOT NULL DEFAULT 48.0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_work_logs_tenant_emp ON working_hour_logs(tenant_id, employee_id, work_date);

ALTER TABLE working_hour_logs ENABLE ROW LEVEL SECURITY;

CREATE POLICY working_hour_logs_tenant_isolation ON working_hour_logs
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Compliance Alerts
CREATE TABLE IF NOT EXISTS compliance_alerts (
    alert_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    employee_id VARCHAR(64) NOT NULL,
    category VARCHAR(64) NOT NULL,
    severity VARCHAR(32) NOT NULL DEFAULT 'WARNING',
    message TEXT NOT NULL,
    is_resolved BOOLEAN NOT NULL DEFAULT FALSE,
    resolved_by VARCHAR(64),
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_comp_alerts_tenant_emp ON compliance_alerts(tenant_id, employee_id);

ALTER TABLE compliance_alerts ENABLE ROW LEVEL SECURITY;

CREATE POLICY compliance_alerts_tenant_isolation ON compliance_alerts
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
