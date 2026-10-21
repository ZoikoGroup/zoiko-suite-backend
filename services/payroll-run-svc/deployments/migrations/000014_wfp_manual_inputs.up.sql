-- ZS-SVC-L-001 WFP-03 — manual payroll inputs with maker-checker (WFP-NP-009).
--
-- A manual input is a one-off earning/deduction a person proposes for ONE worker in ONE period, with a reason
-- and evidence. It is not pay until a DIFFERENT principal approves it, and an approved one reaches payroll only
-- by being pulled into an input batch like every other source (source_kind MANUAL), classified by the same
-- governed code mapping and frozen with the batch. It never bypasses the batch.

-- The mapping and line tables learn the new source kind.
ALTER TABLE wfp_input_code_mappings DROP CONSTRAINT IF EXISTS wfp_map_kind_check;
ALTER TABLE wfp_input_code_mappings ADD CONSTRAINT wfp_map_kind_check CHECK (source_kind IN
    ('BASE_SALARY', 'COMP_COMPONENT', 'COMP_BONUS', 'BENEFIT_EMPLOYEE', 'BENEFIT_EMPLOYER', 'MANUAL'));
ALTER TABLE wfp_payroll_input_lines DROP CONSTRAINT IF EXISTS wfp_in_line_kind_check;
ALTER TABLE wfp_payroll_input_lines ADD CONSTRAINT wfp_in_line_kind_check CHECK (source_kind IN
    ('BASE_SALARY', 'COMP_COMPONENT', 'COMP_BONUS', 'BENEFIT_EMPLOYEE', 'BENEFIT_EMPLOYER', 'MANUAL'));

CREATE TABLE IF NOT EXISTS wfp_manual_inputs (
    manual_id         UUID PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    legal_entity_id   TEXT NOT NULL,
    payroll_period_id TEXT NOT NULL,
    worker_id         UUID NOT NULL,
    input_code        TEXT NOT NULL,
    category          TEXT NOT NULL,
    amount            NUMERIC(18, 4) NOT NULL,
    currency          TEXT NOT NULL,
    effective_date    DATE NOT NULL,
    reason            TEXT NOT NULL,
    evidence_ref      TEXT NOT NULL,
    state             TEXT NOT NULL DEFAULT 'PROPOSED',
    proposed_by       TEXT NOT NULL,
    decided_by        TEXT,
    decided_at        TIMESTAMPTZ,
    decision_note     TEXT,
    correlation_id    TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_manual_state_check CHECK (state IN ('PROPOSED', 'APPROVED', 'REJECTED', 'WITHDRAWN')),
    CONSTRAINT wfp_manual_category_check CHECK (category IN ('EARNING', 'DEDUCTION')),
    CONSTRAINT wfp_manual_amount_check CHECK (amount > 0),
    CONSTRAINT wfp_manual_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wfp_manual_reason_check CHECK (btrim(reason) <> '' AND btrim(evidence_ref) <> '' AND btrim(input_code) <> ''),
    -- The decision is recorded exactly when the input left PROPOSED, and the maker is never the checker.
    CONSTRAINT wfp_manual_decision_check CHECK ((state = 'PROPOSED') = (decided_by IS NULL AND decided_at IS NULL)),
    CONSTRAINT wfp_manual_four_eyes CHECK (state NOT IN ('APPROVED', 'REJECTED') OR decided_by <> proposed_by)
);
CREATE INDEX IF NOT EXISTS idx_wfp_manual_scope ON wfp_manual_inputs (tenant_id, legal_entity_id, payroll_period_id, state);

-- Material fields never change; a PROPOSED input may be decided or withdrawn once; the decision is final.
CREATE OR REPLACE FUNCTION wfp_manual_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'manual payroll inputs are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.manual_id IS DISTINCT FROM OLD.manual_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR NEW.payroll_period_id IS DISTINCT FROM OLD.payroll_period_id
       OR NEW.worker_id IS DISTINCT FROM OLD.worker_id OR NEW.input_code IS DISTINCT FROM OLD.input_code OR NEW.category IS DISTINCT FROM OLD.category
       OR NEW.amount IS DISTINCT FROM OLD.amount OR NEW.currency IS DISTINCT FROM OLD.currency
       OR NEW.effective_date IS DISTINCT FROM OLD.effective_date OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.evidence_ref IS DISTINCT FROM OLD.evidence_ref OR NEW.proposed_by IS DISTINCT FROM OLD.proposed_by THEN
        RAISE EXCEPTION 'a manual input cannot be edited; withdraw it and propose a new one'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> 'PROPOSED' AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION '% is final', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> 'PROPOSED' AND (NEW.decided_by IS DISTINCT FROM OLD.decided_by OR NEW.decided_at IS DISTINCT FROM OLD.decided_at) THEN
        RAISE EXCEPTION 'the decision is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_manual_guard_trg ON wfp_manual_inputs;
CREATE TRIGGER wfp_manual_guard_trg
    BEFORE UPDATE OR DELETE ON wfp_manual_inputs
    FOR EACH ROW EXECUTE FUNCTION wfp_manual_guard();

ALTER TABLE wfp_manual_inputs ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_manual_inputs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_manual_inputs FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
