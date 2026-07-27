-- Schema for reporting-orchestration-svc
CREATE TABLE IF NOT EXISTS report_definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    report_name VARCHAR(128) NOT NULL,
    report_type VARCHAR(64) NOT NULL, -- FINANCIAL_SUMMARY, PAYROLL_SUMMARY, COMPLIANCE_OVERVIEW, AUDIT_TRAIL, CASH_FLOW, WORKFORCE_ANALYTICS
    output_format VARCHAR(32) NOT NULL DEFAULT 'JSON', -- JSON, CSV, PDF
    data_sources TEXT[] NOT NULL DEFAULT '{}',  -- e.g. ARRAY['ledger-svc', 'payroll-svc']
    schedule_cron VARCHAR(128),                  -- e.g. '0 6 * * 1' for every Monday 06:00
    is_scheduled BOOLEAN NOT NULL DEFAULT FALSE,
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, PAUSED, ARCHIVED
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS report_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    definition_id UUID NOT NULL REFERENCES report_definitions(id) ON DELETE CASCADE,
    triggered_by VARCHAR(64) NOT NULL DEFAULT 'MANUAL', -- MANUAL, SCHEDULED, API
    period_start DATE,
    period_end DATE,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING', -- PENDING, RUNNING, COMPLETED, FAILED
    row_count INT NOT NULL DEFAULT 0,
    output_location TEXT,
    error_message TEXT,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enable RLS
ALTER TABLE report_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_runs ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS report_definitions_tenant_isolation ON report_definitions;
DROP POLICY IF EXISTS report_runs_tenant_isolation ON report_runs;

CREATE POLICY report_definitions_tenant_isolation ON report_definitions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY report_runs_tenant_isolation ON report_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_report_definitions_tenant_entity ON report_definitions(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_report_definitions_type ON report_definitions(tenant_id, report_type);
CREATE INDEX IF NOT EXISTS idx_report_runs_tenant_def ON report_runs(tenant_id, definition_id);
CREATE INDEX IF NOT EXISTS idx_report_runs_status ON report_runs(tenant_id, status);
