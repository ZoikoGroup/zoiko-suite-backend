-- 000013_compliance_exception_escalation.up.sql
-- Obligations Service — Compliance Status, Exception Cases, and Escalation Records
-- (Doc 04 §13.1)

BEGIN;

-- ── Compliance Status ──────────────────────────────────────────────────────────
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

ALTER TABLE compliance_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE compliance_status FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON compliance_status;
CREATE POLICY tenant_isolation_policy ON compliance_status FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE INDEX IF NOT EXISTS idx_compliance_status_legal_entity ON compliance_status (legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_compliance_status_domain ON compliance_status (domain_code);
CREATE INDEX IF NOT EXISTS idx_compliance_status_status ON compliance_status (status_code);
CREATE INDEX IF NOT EXISTS idx_compliance_status_evaluated ON compliance_status (evaluated_at DESC);

-- ── Exception Cases ─────────────────────────────────────────────────────────────
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

ALTER TABLE exception_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE exception_cases FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON exception_cases;
CREATE POLICY tenant_isolation_policy ON exception_cases FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE INDEX IF NOT EXISTS idx_exception_cases_legal_entity ON exception_cases (legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_exception_cases_type ON exception_cases (exception_type);
CREATE INDEX IF NOT EXISTS idx_exception_cases_status ON exception_cases (case_status);
CREATE INDEX IF NOT EXISTS idx_exception_cases_linked ON exception_cases (linked_object_type, linked_object_id);
CREATE INDEX IF NOT EXISTS idx_exception_cases_created ON exception_cases (created_at DESC);

-- ── Escalation Records ──────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS escalation_records (
    escalation_record_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID        NOT NULL,
    exception_case_id        UUID        NOT NULL REFERENCES exception_cases(exception_case_id),
    escalated_to_role        VARCHAR(64) NOT NULL,
    escalation_reason        TEXT        NOT NULL,
    escalation_status        VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    escalated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

ALTER TABLE escalation_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE escalation_records FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON escalation_records;
CREATE POLICY tenant_isolation_policy ON escalation_records FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE INDEX IF NOT EXISTS idx_escalation_records_exception_case ON escalation_records (exception_case_id);
CREATE INDEX IF NOT EXISTS idx_escalation_records_status ON escalation_records (escalation_status);
CREATE INDEX IF NOT EXISTS idx_escalation_records_escalated ON escalation_records (escalated_at DESC);

COMMIT;