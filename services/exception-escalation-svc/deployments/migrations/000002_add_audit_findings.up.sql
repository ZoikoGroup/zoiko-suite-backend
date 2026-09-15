-- +migrate Up
BEGIN;

-- AUD-08 Finding/Exception wraps exception_cases via a mandatory
-- exception_case_id FK rather than adding audit-only columns to it — see
-- internal/domain/finding.go's own package doc. tenant_id/ids are TEXT to
-- match this service's own existing convention (exception_cases uses
-- app-generated "excase-<uuid>"-style string ids, not native UUID columns).

CREATE TABLE audit_findings (
    finding_id                      TEXT        NOT NULL,
    exception_case_id               TEXT        NOT NULL,
    tenant_id                       TEXT        NOT NULL,
    legal_entity_id                 TEXT        NOT NULL,
    engagement_id                   TEXT        NOT NULL,
    finding_type                    TEXT        NOT NULL,
    requires_remediation_evidence   BOOLEAN     NOT NULL DEFAULT true,
    status                          TEXT        NOT NULL DEFAULT 'IDENTIFIED',
    reopened_count                  INTEGER     NOT NULL DEFAULT 0,
    created_by_principal_id         TEXT        NOT NULL,
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at                       TIMESTAMPTZ,
    PRIMARY KEY (finding_id, tenant_id),
    UNIQUE (exception_case_id, tenant_id)
);
CREATE INDEX idx_audit_findings_engagement ON audit_findings (tenant_id, engagement_id);

CREATE TABLE misstatement_records (
    misstatement_id           TEXT        NOT NULL,
    finding_id                TEXT        NOT NULL,
    tenant_id                 TEXT        NOT NULL,
    amount                    NUMERIC(20,2) NOT NULL,
    corrects_misstatement_id  TEXT,
    is_corrected              BOOLEAN     NOT NULL DEFAULT false,
    recorded_by_principal_id  TEXT        NOT NULL,
    recorded_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (misstatement_id, tenant_id)
);

CREATE TABLE materiality_evaluations (
    evaluation_id              TEXT        NOT NULL,
    finding_id                 TEXT        NOT NULL,
    tenant_id                  TEXT        NOT NULL,
    version                    INTEGER     NOT NULL,
    is_material                BOOLEAN     NOT NULL,
    qualitative_notes          TEXT        NOT NULL DEFAULT '',
    evaluated_by_principal_id  TEXT        NOT NULL,
    evaluated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (evaluation_id, tenant_id)
);

CREATE TABLE control_deficiency_records (
    deficiency_id              TEXT        NOT NULL,
    finding_id                 TEXT        NOT NULL,
    tenant_id                  TEXT        NOT NULL,
    control_description        TEXT        NOT NULL,
    deficiency_severity        TEXT        NOT NULL,
    recorded_by_principal_id   TEXT        NOT NULL,
    recorded_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (deficiency_id, tenant_id)
);

-- still_impacts_report is independent of the parent finding's own
-- status — see the domain package doc for why this is the AUD-NEG-029
-- mechanism.
CREATE TABLE scope_limitations (
    limitation_id               TEXT        NOT NULL,
    finding_id                  TEXT        NOT NULL,
    tenant_id                   TEXT        NOT NULL,
    description                 TEXT        NOT NULL,
    still_impacts_report        BOOLEAN     NOT NULL DEFAULT true,
    recorded_by_principal_id    TEXT        NOT NULL,
    recorded_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at                    TIMESTAMPTZ,
    PRIMARY KEY (limitation_id, tenant_id)
);
CREATE INDEX idx_scope_limitations_open ON scope_limitations (tenant_id, finding_id) WHERE still_impacts_report = true AND resolved_at IS NULL;

CREATE TABLE management_responses (
    response_id                 TEXT        NOT NULL,
    finding_id                  TEXT        NOT NULL,
    tenant_id                   TEXT        NOT NULL,
    response_text                TEXT        NOT NULL,
    remediation_plan                TEXT        NOT NULL DEFAULT '',
    responded_by_principal_id          TEXT        NOT NULL,
    responded_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (response_id, tenant_id)
);

CREATE TABLE remediation_evidence (
    remediation_id               TEXT        NOT NULL,
    finding_id                   TEXT        NOT NULL,
    tenant_id                    TEXT        NOT NULL,
    evidence_ref                 TEXT        NOT NULL,
    reperformed                   BOOLEAN     NOT NULL DEFAULT false,
    recorded_by_principal_id         TEXT        NOT NULL,
    recorded_at                         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (remediation_id, tenant_id)
);

CREATE TABLE finding_closure_assessments (
    assessment_id               TEXT        NOT NULL,
    finding_id                  TEXT        NOT NULL,
    tenant_id                   TEXT        NOT NULL,
    closed_by_principal_id       TEXT        NOT NULL,
    closure_notes                   TEXT        NOT NULL DEFAULT '',
    closed_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (assessment_id, tenant_id)
);

ALTER TABLE audit_findings ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_findings FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_findings_tenant_isolation ON audit_findings
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE misstatement_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE misstatement_records FORCE ROW LEVEL SECURITY;
CREATE POLICY misstatement_records_tenant_isolation ON misstatement_records
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE materiality_evaluations ENABLE ROW LEVEL SECURITY;
ALTER TABLE materiality_evaluations FORCE ROW LEVEL SECURITY;
CREATE POLICY materiality_evaluations_tenant_isolation ON materiality_evaluations
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE control_deficiency_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_deficiency_records FORCE ROW LEVEL SECURITY;
CREATE POLICY control_deficiency_records_tenant_isolation ON control_deficiency_records
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE scope_limitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE scope_limitations FORCE ROW LEVEL SECURITY;
CREATE POLICY scope_limitations_tenant_isolation ON scope_limitations
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE management_responses ENABLE ROW LEVEL SECURITY;
ALTER TABLE management_responses FORCE ROW LEVEL SECURITY;
CREATE POLICY management_responses_tenant_isolation ON management_responses
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE remediation_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE remediation_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY remediation_evidence_tenant_isolation ON remediation_evidence
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE finding_closure_assessments ENABLE ROW LEVEL SECURITY;
ALTER TABLE finding_closure_assessments FORCE ROW LEVEL SECURITY;
CREATE POLICY finding_closure_assessments_tenant_isolation ON finding_closure_assessments
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- "Original finding immutable": only status/reopened_count/closed_at may
-- ever change on an existing row — every other column, including
-- finding_type and exception_case_id, is permanent once written.
CREATE OR REPLACE FUNCTION reject_finding_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.exception_case_id IS DISTINCT FROM OLD.exception_case_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.engagement_id IS DISTINCT FROM OLD.engagement_id
        OR NEW.finding_type IS DISTINCT FROM OLD.finding_type
        OR NEW.requires_remediation_evidence IS DISTINCT FROM OLD.requires_remediation_evidence
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'original finding content is immutable — only status/reopened_count/closed_at may change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_finding_mutation
    BEFORE UPDATE ON audit_findings
    FOR EACH ROW EXECUTE FUNCTION reject_finding_mutation();
CREATE OR REPLACE FUNCTION reject_finding_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit findings are never deleted';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_finding_delete
    BEFORE DELETE ON audit_findings
    FOR EACH ROW EXECUTE FUNCTION reject_finding_delete();

-- Append-only doctrine for every evidentiary child table — a correction
-- is always a NEW row (AUD-NEG-027), never an edit of the original.
CREATE OR REPLACE FUNCTION reject_audit_finding_child_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_misstatement_mutation
    BEFORE UPDATE OR DELETE ON misstatement_records
    FOR EACH ROW EXECUTE FUNCTION reject_audit_finding_child_mutation();
CREATE TRIGGER trg_reject_materiality_evaluation_mutation
    BEFORE UPDATE OR DELETE ON materiality_evaluations
    FOR EACH ROW EXECUTE FUNCTION reject_audit_finding_child_mutation();
CREATE TRIGGER trg_reject_control_deficiency_mutation
    BEFORE UPDATE OR DELETE ON control_deficiency_records
    FOR EACH ROW EXECUTE FUNCTION reject_audit_finding_child_mutation();
CREATE TRIGGER trg_reject_management_response_mutation
    BEFORE UPDATE OR DELETE ON management_responses
    FOR EACH ROW EXECUTE FUNCTION reject_audit_finding_child_mutation();
CREATE TRIGGER trg_reject_remediation_evidence_mutation
    BEFORE UPDATE OR DELETE ON remediation_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_audit_finding_child_mutation();
CREATE TRIGGER trg_reject_finding_closure_assessment_mutation
    BEFORE UPDATE OR DELETE ON finding_closure_assessments
    FOR EACH ROW EXECUTE FUNCTION reject_audit_finding_child_mutation();

-- scope_limitations allows exactly one controlled UPDATE path (setting
-- resolved_at) — description/still_impacts_report's INITIAL true value
-- and every other column stay fixed; still_impacts_report itself is
-- never flipped directly (only resolved_at marks resolution, preserving
-- the historical fact that it once impacted the report).
CREATE OR REPLACE FUNCTION reject_scope_limitation_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'scope limitations are never deleted';
    END IF;
    IF NEW.finding_id IS DISTINCT FROM OLD.finding_id
        OR NEW.description IS DISTINCT FROM OLD.description
        OR NEW.still_impacts_report IS DISTINCT FROM OLD.still_impacts_report THEN
        RAISE EXCEPTION 'scope limitation content is immutable — only resolved_at may be set';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_scope_limitation_mutation
    BEFORE UPDATE OR DELETE ON scope_limitations
    FOR EACH ROW EXECUTE FUNCTION reject_scope_limitation_mutation();

COMMIT;
