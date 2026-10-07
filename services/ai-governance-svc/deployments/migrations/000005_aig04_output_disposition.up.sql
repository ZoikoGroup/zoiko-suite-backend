-- 000005_aig04_output_disposition.up.sql
-- ZS-SVC-X-001 §7 — AIG-04: Human Oversight, Output Disposition &
-- Decision Boundary Service.
--
-- Additive to AIG-01/02 (000003/000004), which are untouched. This
-- table is the ONLY source of disposition authority for an AI output —
-- NP-39: "AI output marked 'approved' by the model's own text is
-- ignored; only AIG-04's disposition record has authority." There is
-- no column anywhere in this schema for a model-claimed status, and no
-- code path lets one influence disposition_state — the only way a row
-- reaches ACCEPTED/REJECTED is an explicit DecideDisposition call by a
-- real reviewer_principal_id, recorded here.
--
-- execution_ref is a caller-attested pointer to the AI output under
-- review, not a FK to a real execution log — AIG-03 (the execution
-- gateway that would generate real AIExecution rows) was deliberately
-- not built in this codebase (no live model-inference integration
-- exists to govern), so this service accepts the execution reference
-- as evidence, the same honest-scoping already used for AIG-02's
-- ApproveReleaseRequest gate-cleared booleans.
--
-- This service never executes a downstream action itself —
-- downstream_action_ref is a caller-supplied pointer to record WHAT
-- happened elsewhere, never something this table triggers. That
-- absence of any execution code path is what makes INV-20 ("human
-- acceptance does not itself bypass domain/WFC authorization") and
-- NP-38 ("human accepts AI output but payment approval missing -> no
-- payment") true structurally, not just by convention.

CREATE TABLE output_dispositions (
    disposition_id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                       UUID NOT NULL,
    use_case_id                     UUID NOT NULL REFERENCES ai_use_cases(use_case_id),
    execution_ref                   VARCHAR(255) NOT NULL,
    -- §7.1 oversight classes, determined from the use case's own
    -- operational_class at CreateDisposition time (app logic, not a
    -- DB-level derivation) and stored for audit — a later change to
    -- the use case's classification must never silently rewrite a
    -- past disposition's recorded oversight requirement.
    oversight_class                 VARCHAR(2) NOT NULL
        CHECK (oversight_class IN ('O0', 'O1', 'O2', 'O3', 'O4')),
    -- §7.3 Figure 7 state machine. BLOCKED is the validation-failure
    -- branch (unsafe/schema-invalid/policy-breach output, caller-
    -- attested via validation_failure_reason) — independent of
    -- oversight class, and terminal: a blocked output is never
    -- reviewable. DRAFT_ASSISTIVE/REVIEW_REQUIRED are the pre-decision
    -- states (O0/O1 vs O2/O3); ACCEPTED/REJECTED are the human
    -- decision; SUPERSEDED means a later disposition replaced this
    -- one (see superseded_by_disposition_id).
    disposition_state               VARCHAR(16) NOT NULL
        CHECK (disposition_state IN ('BLOCKED', 'DRAFT_ASSISTIVE', 'REVIEW_REQUIRED', 'ACCEPTED', 'REJECTED', 'SUPERSEDED')),
    validation_failure_reason       TEXT,
    required_reviewer_role          VARCHAR(128),
    -- §7.2: reviewer identity/disposition recorded separately from the
    -- AI execution. reviewer_principal_id and decided_at are set
    -- together, exactly once, by DecideDisposition.
    reviewer_principal_id           VARCHAR(255),
    decision_reason                 TEXT,
    downstream_action_ref           VARCHAR(255),
    -- NP-37: "reviewer accepts in <1s -> monitoring signal, not
    -- automatic invalidity." Recorded, never blocking.
    rapid_decision_flag             BOOLEAN NOT NULL DEFAULT false,
    superseded_by_disposition_id    UUID REFERENCES output_dispositions(disposition_id),
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id         VARCHAR(255) NOT NULL,
    decided_at                      TIMESTAMPTZ,
    CHECK ((reviewer_principal_id IS NULL) = (decided_at IS NULL)),
    CHECK (disposition_state <> 'BLOCKED' OR validation_failure_reason IS NOT NULL)
);

CREATE INDEX idx_output_dispositions_tenant_state ON output_dispositions (tenant_id, disposition_state);
CREATE INDEX idx_output_dispositions_use_case ON output_dispositions (use_case_id);
CREATE INDEX idx_output_dispositions_execution_ref ON output_dispositions (tenant_id, execution_ref);

ALTER TABLE output_dispositions ENABLE ROW LEVEL SECURITY;
ALTER TABLE output_dispositions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON output_dispositions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Forward-only per §7.3 Figure 7. BLOCKED and SUPERSEDED are fully
-- terminal; ACCEPTED/REJECTED can only move to SUPERSEDED (a later
-- disposition replacing this one), never back to a pre-decision state
-- — a decision, once made, is never silently reopened.
CREATE OR REPLACE FUNCTION aig04_enforce_disposition_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'output_dispositions rows cannot be deleted';
    END IF;
    IF OLD.disposition_state IN ('BLOCKED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'output_disposition % is % and immutable', OLD.disposition_id, OLD.disposition_state;
    END IF;
    IF OLD.disposition_state <> NEW.disposition_state THEN
        CASE OLD.disposition_state
            WHEN 'DRAFT_ASSISTIVE' THEN
                IF NEW.disposition_state NOT IN ('ACCEPTED', 'REJECTED') THEN
                    RAISE EXCEPTION 'invalid output_disposition transition from DRAFT_ASSISTIVE to %', NEW.disposition_state;
                END IF;
            WHEN 'REVIEW_REQUIRED' THEN
                IF NEW.disposition_state NOT IN ('ACCEPTED', 'REJECTED') THEN
                    RAISE EXCEPTION 'invalid output_disposition transition from REVIEW_REQUIRED to %', NEW.disposition_state;
                END IF;
            WHEN 'ACCEPTED' THEN
                IF NEW.disposition_state <> 'SUPERSEDED' THEN
                    RAISE EXCEPTION 'invalid output_disposition transition from ACCEPTED to %', NEW.disposition_state;
                END IF;
            WHEN 'REJECTED' THEN
                IF NEW.disposition_state <> 'SUPERSEDED' THEN
                    RAISE EXCEPTION 'invalid output_disposition transition from REJECTED to %', NEW.disposition_state;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown output_disposition state %', OLD.disposition_state;
        END CASE;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.use_case_id IS DISTINCT FROM OLD.use_case_id
        OR NEW.execution_ref IS DISTINCT FROM OLD.execution_ref
        OR NEW.oversight_class IS DISTINCT FROM OLD.oversight_class
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'output_disposition % identity/oversight fields are immutable', OLD.disposition_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_disposition_lifecycle
    BEFORE UPDATE OR DELETE ON output_dispositions
    FOR EACH ROW EXECUTE FUNCTION aig04_enforce_disposition_lifecycle();
