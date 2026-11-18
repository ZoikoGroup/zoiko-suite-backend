-- 000003_compliance_status.up.sql
-- Compliance Status Service — Doc 04 §13.1 ComplianceStatus entity
-- Replaces compliance_status_records and compliance_gaps with authoritative ComplianceStatus table

BEGIN;

CREATE TABLE IF NOT EXISTS compliance_status (
    compliance_status_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID        NOT NULL,
    legal_entity_id            UUID        NOT NULL,
    domain_code                VARCHAR(32) NOT NULL,
    status_code                VARCHAR(32) NOT NULL,
    status_reason              TEXT        NOT NULL,
    evaluated_at               TIMESTAMPTZ NOT NULL,
    evidence_sufficiency_state VARCHAR(32) NOT NULL,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id    TEXT        NOT NULL,
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version                    INTEGER     NOT NULL DEFAULT 1
);

-- Row-Level Security
ALTER TABLE compliance_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE compliance_status FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON compliance_status;
CREATE POLICY tenant_isolation_policy ON compliance_status FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_compliance_status_legal_entity ON compliance_status (legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_compliance_status_domain ON compliance_status (domain_code);
CREATE INDEX IF NOT EXISTS idx_compliance_status_status ON compliance_status (status_code);
CREATE INDEX IF NOT EXISTS idx_compliance_status_evaluated ON compliance_status (evaluated_at DESC);

-- Constraints for valid enum values
ALTER TABLE compliance_status DROP CONSTRAINT IF EXISTS chk_compliance_status_status_code;
ALTER TABLE compliance_status ADD CONSTRAINT chk_compliance_status_status_code
    CHECK (status_code IN ('COMPLIANT', 'NON_COMPLIANT', 'AT_RISK', 'UNKNOWN', 'WAIVED'))
    NOT VALID;

ALTER TABLE compliance_status DROP CONSTRAINT IF EXISTS chk_compliance_status_evidence_state;
ALTER TABLE compliance_status ADD CONSTRAINT chk_compliance_status_evidence_state
    CHECK (evidence_sufficiency_state IN ('SUFFICIENT', 'INSUFFICIENT', 'PENDING', 'NOT_APPLICABLE'))
    NOT VALID;

ALTER TABLE compliance_status DROP CONSTRAINT IF EXISTS chk_compliance_status_domain_code;
ALTER TABLE compliance_status ADD CONSTRAINT chk_compliance_status_domain_code
    CHECK (domain_code IN ('TAX', 'PAYROLL', 'HR', 'FINANCE', 'LEGAL', 'COMMERCIAL'))
    NOT VALID;

-- Migrate data from old tables if they exist
-- (Run after 000001_initial_schema.up.sql)
DO $$
BEGIN
    -- Migrate compliance_status_records to compliance_status
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'compliance_status_records') THEN
        INSERT INTO compliance_status (
            compliance_status_id, tenant_id, legal_entity_id, domain_code, status_code,
            status_reason, evaluated_at, evidence_sufficiency_state,
            created_at, created_by_principal_id, updated_at, version
        )
        SELECT
            'cstat-' || md5(status_id || tenant_id)::uuid,
            tenant_id::uuid,
            legal_entity_id::uuid,
            CASE domain_name
                WHEN 'TAX' THEN 'TAX' WHEN 'PAYROLL' THEN 'PAYROLL' WHEN 'HR' THEN 'HR'
                WHEN 'FINANCE' THEN 'FINANCE' WHEN 'LEGAL' THEN 'LEGAL' WHEN 'COMMERCIAL' THEN 'COMMERCIAL'
                ELSE 'TAX'
            END,
            CASE overall_status
                WHEN 'COMPLIANT' THEN 'COMPLIANT'
                WHEN 'NON_COMPLIANT' THEN 'NON_COMPLIANT'
                WHEN 'WARNING' THEN 'AT_RISK'
                WHEN 'UNDER_REVIEW' THEN 'AT_RISK'
                ELSE 'UNKNOWN'
            END,
            COALESCE(notes, 'Migrated from legacy record'),
            last_evaluated_at,
            CASE
                WHEN overdue_obligations > 0 OR open_exceptions > 0 THEN 'INSUFFICIENT'
                WHEN total_obligations = 0 THEN 'NOT_APPLICABLE'
                ELSE 'SUFFICIENT'
            END,
            created_at,
            created_by,
            updated_at,
            1
        FROM compliance_status_records
        ON CONFLICT (compliance_status_id) DO NOTHING;
    END IF;

    -- Migrate compliance_gaps to exception_cases (in exception-escalation-svc)
    -- This will be handled by exception-escalation-svc migration
END $$;

COMMIT;