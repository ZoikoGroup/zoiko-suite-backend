-- Migration: 000005_add_milestones.up.sql
--
-- PRJ-03 milestone-based revenue recognition. Adds MILESTONE to the allowed
-- recognition_method set (PERCENTAGE_OF_COMPLETION behaviour is untouched)
-- and the two tables the method needs:
--
--   project_milestones                    a project's contractual milestones;
--                                         a milestone only counts toward
--                                         revenue once it is ACHIEVED (with
--                                         evidence) AND that achievement has
--                                         been APPROVED by a DIFFERENT
--                                         principal (SoD).
--   project_recognition_run_milestones    insert-only evidence: exactly which
--                                         milestones (and amounts) a run's
--                                         cumulative revenue was built from,
--                                         so the run is reconstructable.
--
-- Over-contract-value cannot be enforced here: contract_value is declared
-- per recognition run, not per milestone. FreezeAndCalculate refuses a run
-- whose approved-achieved milestone total exceeds that run's contract_value.

ALTER TABLE project_financial_profiles DROP CONSTRAINT chk_recognition_method;
ALTER TABLE project_financial_profiles ADD CONSTRAINT chk_recognition_method CHECK (
    recognition_method IN ('PERCENTAGE_OF_COMPLETION', 'COMPLETED_CONTRACT', 'TIME_AND_MATERIALS', 'MILESTONE'));

CREATE TABLE project_milestones (
    milestone_id                  UUID PRIMARY KEY,
    tenant_id                     VARCHAR(255) NOT NULL,
    project_id                    UUID NOT NULL REFERENCES projects(project_id),
    name                          VARCHAR(255) NOT NULL,
    amount                        NUMERIC(18,2) NOT NULL,
    status                        VARCHAR(20) NOT NULL DEFAULT 'PLANNED',
    achievement_evidence_ref      VARCHAR(512),
    achieved_at                   TIMESTAMP WITH TIME ZONE,
    achieved_by_principal_id      VARCHAR(255),
    approved_at                   TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id      VARCHAR(255),
    created_at                    TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id       VARCHAR(255) NOT NULL,

    CONSTRAINT chk_milestone_amount_positive CHECK (amount > 0),
    CONSTRAINT chk_milestone_status CHECK (status IN ('PLANNED', 'ACHIEVED')),
    CONSTRAINT uq_milestone_name UNIQUE (tenant_id, project_id, name),
    -- ACHIEVED requires evidence, a timestamp and who marked it.
    CONSTRAINT chk_milestone_achieved_complete CHECK (
        status <> 'ACHIEVED' OR (
            achievement_evidence_ref IS NOT NULL AND btrim(achievement_evidence_ref) <> ''
            AND achieved_at IS NOT NULL AND achieved_by_principal_id IS NOT NULL)),
    -- Approval only on an ACHIEVED milestone, with an approver who is not
    -- the principal that marked it achieved (backstop to the store check).
    CONSTRAINT chk_milestone_approval_sod CHECK (
        approved_at IS NULL OR (
            status = 'ACHIEVED' AND approved_by_principal_id IS NOT NULL
            AND approved_by_principal_id <> achieved_by_principal_id))
);

CREATE INDEX idx_project_milestones_project ON project_milestones (tenant_id, project_id);

-- Once ACHIEVED, the economic fields can never change; once approved, no
-- field can change and the row cannot be deleted.
CREATE OR REPLACE FUNCTION reject_milestone_economic_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status = 'ACHIEVED' THEN
            RAISE EXCEPTION 'project_milestones: an achieved milestone cannot be deleted';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.status = 'ACHIEVED' AND (
        NEW.amount IS DISTINCT FROM OLD.amount OR
        NEW.name IS DISTINCT FROM OLD.name OR
        NEW.project_id IS DISTINCT FROM OLD.project_id OR
        NEW.achievement_evidence_ref IS DISTINCT FROM OLD.achievement_evidence_ref OR
        NEW.status IS DISTINCT FROM OLD.status OR
        NEW.achieved_at IS DISTINCT FROM OLD.achieved_at OR
        NEW.achieved_by_principal_id IS DISTINCT FROM OLD.achieved_by_principal_id
    ) THEN
        RAISE EXCEPTION 'project_milestones: economic fields are immutable once ACHIEVED';
    END IF;
    IF OLD.approved_at IS NOT NULL AND (
        NEW.approved_at IS DISTINCT FROM OLD.approved_at OR
        NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id
    ) THEN
        RAISE EXCEPTION 'project_milestones: approval is immutable once recorded';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_milestone_economic_mutation
    BEFORE UPDATE OR DELETE ON project_milestones
    FOR EACH ROW EXECUTE FUNCTION reject_milestone_economic_mutation();

CREATE TABLE project_recognition_run_milestones (
    run_id          UUID NOT NULL REFERENCES project_recognition_runs(run_id),
    milestone_id    UUID NOT NULL REFERENCES project_milestones(milestone_id),
    tenant_id       VARCHAR(255) NOT NULL,
    amount          NUMERIC(18,2) NOT NULL,   -- the amount counted at freeze
    included_at     TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY (run_id, milestone_id)
);

CREATE OR REPLACE FUNCTION reject_run_milestone_evidence_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'project_recognition_run_milestones is insert-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_run_milestone_evidence_mutation
    BEFORE UPDATE OR DELETE ON project_recognition_run_milestones
    FOR EACH ROW EXECUTE FUNCTION reject_run_milestone_evidence_mutation();

ALTER TABLE project_milestones ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_milestones FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_milestones
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE project_recognition_run_milestones ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_recognition_run_milestones FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_recognition_run_milestones
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_run_milestones_run ON project_recognition_run_milestones (tenant_id, run_id);
