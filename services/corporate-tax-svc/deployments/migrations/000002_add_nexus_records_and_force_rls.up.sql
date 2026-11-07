-- +migrate Up
BEGIN;

-- 1. Priority 3: Add FORCE ROW LEVEL SECURITY to corporate_tax_returns
ALTER TABLE corporate_tax_returns FORCE ROW LEVEL SECURITY;

-- 2. Priority 6 Row 73: Add missing NexusRecord entity per Doc 04 §13 / §1878
CREATE TABLE IF NOT EXISTS nexus_records (
    nexus_record_id  TEXT          NOT NULL,
    tenant_id        TEXT          NOT NULL,
    legal_entity_id  TEXT          NOT NULL,
    jurisdiction_id  TEXT          NOT NULL,
    nexus_type       TEXT          NOT NULL,
    threshold_amount NUMERIC(24,4) NOT NULL DEFAULT 0,
    currency         TEXT          NOT NULL DEFAULT 'USD',
    status           TEXT          NOT NULL DEFAULT 'ACTIVE',
    effective_from   DATE          NOT NULL,
    effective_to     DATE,
    created_by       TEXT          NOT NULL,
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (nexus_record_id, tenant_id),
    CONSTRAINT uk_nexus_records UNIQUE (tenant_id, legal_entity_id, jurisdiction_id, nexus_type, effective_from)
);

-- Row-Level Security
ALTER TABLE nexus_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE nexus_records FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS nexus_records_tenant_isolation ON nexus_records;
CREATE POLICY nexus_records_tenant_isolation ON nexus_records
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_nexus_records_tenant_entity ON nexus_records (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_nexus_records_jurisdiction   ON nexus_records (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_nexus_records_type           ON nexus_records (nexus_type);
CREATE INDEX IF NOT EXISTS idx_nexus_records_status         ON nexus_records (status);

COMMIT;

