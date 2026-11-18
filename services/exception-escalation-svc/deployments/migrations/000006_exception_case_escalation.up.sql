-- 000006_exception_case_escalation.up.sql
-- Exception & Escalation Service — Doc 04 §13.1 ExceptionCase and EscalationRecord entities
-- Aligns existing tables to authoritative schema

BEGIN;

-- Drop and recreate exception_cases with Doc 04 §13.1 schema
DROP TABLE IF EXISTS exception_cases CASCADE;

CREATE TABLE IF NOT EXISTS exception_cases (
    exception_case_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID        NOT NULL,
    legal_entity_id        UUID        NOT NULL,
    exception_type         VARCHAR(64) NOT NULL,
    severity_level         VARCHAR(32) NOT NULL,
    linked_object_type     VARCHAR(32) NOT NULL,
    linked_object_id       UUID        NOT NULL,
    case_status            VARCHAR(32) NOT NULL DEFAULT 'OPEN',
    escalated_at           TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT       NOT NULL,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version                INTEGER     NOT NULL DEFAULT 1
);

-- Drop and recreate escalation_records with Doc 04 §13.1 schema
DROP TABLE IF EXISTS escalation_records CASCADE;

CREATE TABLE IF NOT EXISTS escalation_records (
    escalation_record_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    exception_case_id      UUID        NOT NULL REFERENCES exception_cases(exception_case_id),
    escalated_to_role      VARCHAR(64) NOT NULL,
    escalation_reason      TEXT        NOT NULL,
    escalation_status      VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    escalated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT       NOT NULL
);

-- Row-Level Security
ALTER TABLE exception_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE exception_cases FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON exception_cases;
CREATE POLICY tenant_isolation_policy ON exception_cases FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE escalation_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE escalation_records FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON escalation_records;
CREATE POLICY tenant_isolation_policy ON escalation_records FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_exception_cases_legal_entity ON exception_cases (legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_exception_cases_type ON exception_cases (exception_type);
CREATE INDEX IF NOT EXISTS idx_exception_cases_status ON exception_cases (case_status);
CREATE INDEX IF NOT EXISTS idx_exception_cases_linked ON exception_cases (linked_object_type, linked_object_id);
CREATE INDEX IF NOT EXISTS idx_exception_cases_created ON exception_cases (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_escalation_records_exception_case ON escalation_records (exception_case_id);
CREATE INDEX IF NOT EXISTS idx_escalation_records_status ON escalation_records (escalation_status);
CREATE INDEX IF NOT EXISTS idx_escalation_records_escalated ON escalation_records (escalated_at DESC);

-- Constraints for valid enum values
ALTER TABLE exception_cases DROP CONSTRAINT IF EXISTS chk_exception_cases_exception_type;
ALTER TABLE exception_cases ADD CONSTRAINT chk_exception_cases_exception_type
    CHECK (exception_type IN ('MISSED_DEADLINE', 'INSUFFICIENT_EVIDENCE', 'RULE_CONFLICT', 'JURISDICTION_CHANGE', 'DATA_QUALITY'))
    NOT VALID;

ALTER TABLE exception_cases DROP CONSTRAINT IF EXISTS chk_exception_cases_severity;
ALTER TABLE exception_cases ADD CONSTRAINT chk_exception_cases_severity
    CHECK (severity_level IN ('CRITICAL', 'HIGH', 'MEDIUM', 'LOW'))
    NOT VALID;

ALTER TABLE exception_cases DROP CONSTRAINT IF EXISTS chk_exception_cases_linked_object_type;
ALTER TABLE exception_cases ADD CONSTRAINT chk_exception_cases_linked_object_type
    CHECK (linked_object_type IN ('OBLIGATION', 'FILING', 'POLICY', 'CONTRACT'))
    NOT VALID;

ALTER TABLE exception_cases DROP CONSTRAINT IF EXISTS chk_exception_cases_case_status;
ALTER TABLE exception_cases ADD CONSTRAINT chk_exception_cases_case_status
    CHECK (case_status IN ('OPEN', 'INVESTIGATING', 'RESOLVED', 'ESCALATED', 'CLOSED'))
    NOT VALID;

ALTER TABLE escalation_records DROP CONSTRAINT IF EXISTS chk_escalation_records_status;
ALTER TABLE escalation_records ADD CONSTRAINT chk_escalation_records_status
    CHECK (escalation_status IN ('PENDING', 'ACKNOWLEDGED', 'RESOLVED', 'EXPIRED'))
    NOT VALID;

-- Migrate data from old schema if tables existed with old structure
-- (Run after previous migrations)
DO $$
BEGIN
    -- Note: This migration drops and recreates tables, so data migration
    -- from previous schema versions would need to be handled separately
    -- if needed. The old schema used TEXT primary keys and different enums.
END $$;

COMMIT;