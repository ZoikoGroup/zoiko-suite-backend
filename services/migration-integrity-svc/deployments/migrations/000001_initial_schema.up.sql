-- Schema for migration-integrity-svc
CREATE TABLE IF NOT EXISTS migration_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    migration_name VARCHAR(256) NOT NULL,
    source_system VARCHAR(128) NOT NULL,      -- e.g. LEGACY_ERP, EXTERNAL_PAYROLL, EXCEL_IMPORT
    target_service VARCHAR(128) NOT NULL,     -- e.g. ledger-svc, payroll-svc, workforce-svc
    total_records_count INT NOT NULL DEFAULT 0,
    valid_records_count INT NOT NULL DEFAULT 0,
    invalid_records_count INT NOT NULL DEFAULT 0,
    integrity_score NUMERIC(5,2) NOT NULL DEFAULT 0.00, -- 0.00 to 100.00%
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING', -- PENDING, VALIDATING, COMPLETED, FAILED, ARCHIVED
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS migration_integrity_checks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    job_id UUID NOT NULL REFERENCES migration_jobs(id) ON DELETE CASCADE,
    check_name VARCHAR(128) NOT NULL,
    check_type VARCHAR(64) NOT NULL, -- SCHEMA_VALIDATION, REFERENTIAL_INTEGRITY, DUPLICATE_DETECTION, RANGE_CHECK, FORMAT_CHECK
    records_checked INT NOT NULL DEFAULT 0,
    records_passed INT NOT NULL DEFAULT 0,
    records_failed INT NOT NULL DEFAULT 0,
    severity VARCHAR(32) NOT NULL DEFAULT 'INFO', -- INFO, WARNING, CRITICAL
    detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS migration_audit_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    job_id UUID NOT NULL REFERENCES migration_jobs(id) ON DELETE CASCADE,
    record_ref VARCHAR(256) NOT NULL,
    field_name VARCHAR(128),
    source_value TEXT,
    target_value TEXT,
    violation_type VARCHAR(64) NOT NULL, -- MISSING_REQUIRED, TYPE_MISMATCH, DUPLICATE, OUT_OF_RANGE, ORPHANED_REFERENCE
    is_remediated BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enable RLS
ALTER TABLE migration_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_integrity_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_audit_entries ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS migration_jobs_isolation ON migration_jobs;
DROP POLICY IF EXISTS migration_integrity_checks_isolation ON migration_integrity_checks;
DROP POLICY IF EXISTS migration_audit_entries_isolation ON migration_audit_entries;

CREATE POLICY migration_jobs_isolation ON migration_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY migration_integrity_checks_isolation ON migration_integrity_checks
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY migration_audit_entries_isolation ON migration_audit_entries
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_migration_jobs_tenant_entity ON migration_jobs(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_migration_jobs_status ON migration_jobs(tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_migration_checks_job ON migration_integrity_checks(tenant_id, job_id);
CREATE INDEX IF NOT EXISTS idx_migration_audit_job ON migration_audit_entries(tenant_id, job_id);
