-- Schema for reconciliation-intelligence-svc
CREATE TABLE IF NOT EXISTS reconciliation_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    job_name VARCHAR(128) NOT NULL,
    source_system_a VARCHAR(64) NOT NULL, -- GENERAL_LEDGER, BANK_STATEMENTS, INVOICES, PAYROLL_JOURNAL
    source_system_b VARCHAR(64) NOT NULL,
    total_processed_count INT NOT NULL DEFAULT 0,
    matched_count INT NOT NULL DEFAULT 0,
    unmatched_count INT NOT NULL DEFAULT 0,
    reconciliation_rate NUMERIC(5,2) NOT NULL DEFAULT 0.00, -- 0.00 to 100.00%
    status VARCHAR(32) NOT NULL DEFAULT 'COMPLETED', -- ANALYZING, COMPLETED, ARCHIVED
    analyzed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS reconciliation_unmatched_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    job_id UUID NOT NULL REFERENCES reconciliation_jobs(id) ON DELETE CASCADE,
    transaction_ref_a VARCHAR(128) NOT NULL,
    transaction_ref_b VARCHAR(128),
    amount_a NUMERIC(18,4) NOT NULL,
    amount_b NUMERIC(18,4) DEFAULT 0.00,
    discrepancy_amount NUMERIC(18,4) NOT NULL,
    discrepancy_type VARCHAR(64) NOT NULL, -- AMOUNT_MISMATCH, DATE_SKEW, MISSING_REFERENCE, DUPLICATE_ENTRY
    confidence_score NUMERIC(5,2) NOT NULL, -- 0.00 to 100.00%
    recommendation VARCHAR(64) NOT NULL, -- AUTO_MATCH, WRITE_OFF, TIMING_ADJUSTMENT, MANUAL_REVIEW
    resolution_status VARCHAR(32) NOT NULL DEFAULT 'RECOMMENDED', -- RECOMMENDED, APPROVED, REJECTED, EXECUTED
    resolution_notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enable RLS
ALTER TABLE reconciliation_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_unmatched_items ENABLE ROW LEVEL SECURITY;

-- Drop policies if exist to ensure clean setup
DROP POLICY IF EXISTS reconciliation_jobs_tenant_isolation ON reconciliation_jobs;
DROP POLICY IF EXISTS reconciliation_unmatched_items_tenant_isolation ON reconciliation_unmatched_items;

-- Create Tenant RLS policies
CREATE POLICY reconciliation_jobs_tenant_isolation ON reconciliation_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY reconciliation_unmatched_items_tenant_isolation ON reconciliation_unmatched_items
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_reconciliation_jobs_tenant_entity ON reconciliation_jobs(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_reconciliation_unmatched_job ON reconciliation_unmatched_items(tenant_id, job_id);
CREATE INDEX IF NOT EXISTS idx_reconciliation_unmatched_status ON reconciliation_unmatched_items(tenant_id, resolution_status);
