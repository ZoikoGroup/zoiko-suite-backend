-- Migration: 000007_exception_governance_materiality.up.sql
--
-- A4  Materiality is per currency (materiality_policy) and used: every
--     exception's severity is derived from its exposure against it, and the
--     unresolved exposure is assessed in aggregate. The single, currency-less
--     materiality_amount_minor_units stops being written.
-- A7  Exceptions carry category, assertion, severity, materiality, exposure,
--     owner role / owner, due date, and follow the lifecycle OPEN →
--     INVESTIGATING → REMEDIATED, with ACCEPTED_RISK under a named authority
--     and CLOSED only through a manual match. Every move is recorded in the
--     append-only reconciliation_exception_transitions.
--     Accepted risk leaves an exception unresolved: the run's result stays
--     VARIANCE. A certification over accepted risk is possible only where the
--     run's pinned certification_profile permits it, and it then records the
--     result VARIANCE and the accepted exceptions (certifications.assessment).
--
-- As in 000004/000005, rows written earlier keep their values: the new
-- requirements are NOT VALID constraints, enforced for every new or updated row.

-- ── Definitions ─────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_definitions
    ADD COLUMN materiality_policy    JSONB,
    ADD COLUMN exception_policy      JSONB,
    ADD COLUMN certification_profile JSONB,
    ALTER COLUMN materiality_amount_minor_units DROP NOT NULL;

ALTER TABLE reconciliation_definitions
    ADD CONSTRAINT reconciliation_definitions_governance_check CHECK (
        jsonb_typeof(materiality_policy) = 'object' AND jsonb_typeof(exception_policy) = 'object'
        AND jsonb_typeof(certification_profile) = 'object' AND materiality_amount_minor_units IS NULL
    ) NOT VALID;

CREATE OR REPLACE FUNCTION enforce_definition_versioning()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation definitions cannot be deleted';
    END IF;
    IF NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id THEN
        RAISE EXCEPTION 'reconciliation definition % cannot change legal_entity_id', OLD.definition_id;
    END IF;
    IF (NEW.match_key_fields, NEW.tolerance_amount_minor_units, NEW.materiality_amount_minor_units,
        NEW.amount_scale, NEW.populations, NEW.tolerance_policy, NEW.balance_dimensions,
        NEW.materiality_policy, NEW.exception_policy, NEW.certification_profile)
       IS DISTINCT FROM
       (OLD.match_key_fields, OLD.tolerance_amount_minor_units, OLD.materiality_amount_minor_units,
        OLD.amount_scale, OLD.populations, OLD.tolerance_policy, OLD.balance_dimensions,
        OLD.materiality_policy, OLD.exception_policy, OLD.certification_profile) THEN
        NEW.version := OLD.version + 1;
        NEW.updated_at := NOW();
    ELSE
        NEW.version := OLD.version;
        NEW.updated_at := OLD.updated_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── Runs ────────────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_runs
    ADD COLUMN materiality_policy    JSONB,
    ADD COLUMN exception_policy      JSONB,
    ADD COLUMN certification_profile JSONB,
    ALTER COLUMN materiality_amount_minor_units DROP NOT NULL;

ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_governance_check CHECK (
        jsonb_typeof(materiality_policy) = 'object' AND jsonb_typeof(exception_policy) = 'object'
        AND jsonb_typeof(certification_profile) = 'object' AND materiality_amount_minor_units IS NULL
    ) NOT VALID;

-- The governance policies are pinned from creation, like every other rule
-- (B16). A separate trigger, so 000006's lifecycle function is unchanged.
CREATE OR REPLACE FUNCTION enforce_run_governance_pinned()
RETURNS trigger AS $$
BEGIN
    IF NEW.materiality_policy IS DISTINCT FROM OLD.materiality_policy
        OR NEW.exception_policy IS DISTINCT FROM OLD.exception_policy
        OR NEW.certification_profile IS DISTINCT FROM OLD.certification_profile
    THEN
        RAISE EXCEPTION 'reconciliation run % cannot change its pinned scope or rules', OLD.run_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_run_governance_pinned
    BEFORE UPDATE ON reconciliation_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_run_governance_pinned();

-- A VARIANCE may be ELIGIBLE or CERTIFIED only on a run whose pinned profile
-- permits accepted risk; the application additionally requires every
-- unresolved exception to be accepted risk, immaterial in aggregate, and
-- re-checks that under the run's lock at certification.
ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_state_dimensions_check;
ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_state_dimensions_check CHECK (
        ((status = 'Certified') = (certification_state = 'CERTIFIED'))
        AND (certification_state NOT IN ('ELIGIBLE', 'CERTIFIED') OR result_state = 'MATCH'
             OR (result_state = 'VARIANCE' AND COALESCE((certification_profile ->> 'permit_accepted_risk')::boolean, FALSE)))
        AND ((status = 'Superseded') = (result_state = 'SUPERSEDED'))
        AND (certification_state <> 'SUPERSEDED' OR status = 'Superseded')
        AND (status <> 'Planned' OR (result_state = 'NOT_RUN' AND certification_state = 'UNCERTIFIED'))
        AND (status <> 'ExceptionsOpen' OR result_state = 'VARIANCE')
        AND (status <> 'Failed' OR (result_state = 'INDETERMINATE' AND failure_code IS NOT NULL))
        AND ((failure_code IS NULL) = (failure_reason IS NULL))
        AND (failure_code IS NULL OR status IN ('Failed', 'Superseded'))
    );

-- ── Exceptions ──────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_exceptions DISABLE TRIGGER trigger_exception_resolution;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_status_check;
UPDATE reconciliation_exceptions SET status = CASE status WHEN 'Open' THEN 'OPEN' WHEN 'Resolved' THEN 'CLOSED' ELSE status END;
ALTER TABLE reconciliation_exceptions ENABLE TRIGGER trigger_exception_resolution;

ALTER TABLE reconciliation_exceptions
    ALTER COLUMN status SET DEFAULT 'OPEN',
    ADD CONSTRAINT reconciliation_exceptions_status_check
        CHECK (status IN ('OPEN', 'INVESTIGATING', 'REMEDIATED', 'ACCEPTED_RISK', 'CLOSED')),
    ADD COLUMN category        VARCHAR(32) CHECK (category IN ('MISSING', 'DUPLICATE', 'MISMATCH', 'TIMING', 'VALUATION',
        'CLASSIFICATION', 'AUTHORIZATION', 'DATA_QUALITY', 'EXTERNAL_STATUS', 'LATE_DATA', 'CONTROL_EXECUTION_FAILURE')),
    ADD COLUMN assertion       VARCHAR(32) CHECK (assertion IN ('COMPLETENESS', 'ACCURACY', 'EXISTENCE', 'VALUATION',
        'RIGHTS_OBLIGATIONS', 'CUTOFF', 'CLASSIFICATION', 'PRESENTATION')),
    ADD COLUMN severity        VARCHAR(16) CHECK (severity IN ('INFO', 'WARNING', 'ERROR', 'BLOCKER')),
    ADD COLUMN materiality     VARCHAR(16) CHECK (materiality IN ('MATERIAL', 'IMMATERIAL', 'UNASSESSED')),
    ADD COLUMN exposure_minor_units BIGINT CHECK (exposure_minor_units >= 0),
    ADD COLUMN owner_role      VARCHAR(64),
    ADD COLUMN owner_principal VARCHAR(255),
    ADD COLUMN due_at          TIMESTAMPTZ,
    ADD COLUMN authority_ref   VARCHAR(500),
    ADD COLUMN evidence_ref    VARCHAR(500),
    ADD COLUMN root_cause_code VARCHAR(48),
    ADD COLUMN root_cause_note TEXT,
    ADD COLUMN accepted_by     VARCHAR(255),
    ADD COLUMN accepted_at     TIMESTAMPTZ;

ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_governance_check CHECK (
        category IS NOT NULL AND assertion IS NOT NULL AND severity IS NOT NULL AND materiality IS NOT NULL
        AND exposure_minor_units IS NOT NULL AND owner_role IS NOT NULL AND due_at IS NOT NULL
    ) NOT VALID;

-- Valid for every row: the state each status needs is present.
ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_resolution_check CHECK (
        (status <> 'CLOSED' OR resolved_by_match_id IS NOT NULL)
        AND (status <> 'ACCEPTED_RISK' OR (authority_ref IS NOT NULL AND accepted_by IS NOT NULL AND accepted_at IS NOT NULL))
        AND (status <> 'REMEDIATED' OR evidence_ref IS NOT NULL)
    );

CREATE INDEX IF NOT EXISTS idx_reconciliation_exceptions_owner
    ON reconciliation_exceptions (tenant_id, owner_principal) WHERE status IN ('OPEN', 'INVESTIGATING', 'REMEDIATED');

-- The lifecycle: CLOSED and ACCEPTED_RISK are terminal; everything that
-- identifies or assesses the exception is fixed once raised (DQC: "severity
-- cannot be edited retroactively"); only the lifecycle and its evidence move.
CREATE OR REPLACE FUNCTION enforce_exception_resolution()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation exceptions cannot be deleted';
    END IF;

    IF OLD.status IN ('CLOSED', 'ACCEPTED_RISK') THEN
        RAISE EXCEPTION 'reconciliation exception % is % and immutable', OLD.exception_id, OLD.status;
    END IF;

    IF NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.item_id IS DISTINCT FROM OLD.item_id
        OR NEW.side IS DISTINCT FROM OLD.side
        OR NEW.ref_id IS DISTINCT FROM OLD.ref_id
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR NEW.amount_minor_units IS DISTINCT FROM OLD.amount_minor_units
        OR NEW.raised_at IS DISTINCT FROM OLD.raised_at
        OR NEW.raised_by IS DISTINCT FROM OLD.raised_by
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.dimension IS DISTINCT FROM OLD.dimension
        OR NEW.dimension_value IS DISTINCT FROM OLD.dimension_value
        OR NEW.detail IS DISTINCT FROM OLD.detail
        OR NEW.category IS DISTINCT FROM OLD.category
        OR NEW.assertion IS DISTINCT FROM OLD.assertion
        OR NEW.severity IS DISTINCT FROM OLD.severity
        OR NEW.materiality IS DISTINCT FROM OLD.materiality
        OR NEW.exposure_minor_units IS DISTINCT FROM OLD.exposure_minor_units
        OR NEW.owner_role IS DISTINCT FROM OLD.owner_role
        OR NEW.due_at IS DISTINCT FROM OLD.due_at
    THEN
        RAISE EXCEPTION 'reconciliation exception % may only change its lifecycle, owner and evidence', OLD.exception_id;
    END IF;

    IF NEW.status <> OLD.status THEN
        IF NOT (
            (OLD.status = 'OPEN' AND NEW.status IN ('INVESTIGATING', 'ACCEPTED_RISK', 'CLOSED'))
            OR (OLD.status = 'INVESTIGATING' AND NEW.status IN ('REMEDIATED', 'ACCEPTED_RISK', 'CLOSED'))
            OR (OLD.status = 'REMEDIATED' AND NEW.status IN ('INVESTIGATING', 'CLOSED'))
        ) THEN
            RAISE EXCEPTION 'invalid reconciliation exception transition from % to %', OLD.status, NEW.status;
        END IF;
    END IF;

    IF NEW.resolved_by_match_id IS DISTINCT FROM OLD.resolved_by_match_id AND NEW.status <> 'CLOSED' THEN
        RAISE EXCEPTION 'resolved_by_match_id is set only when a manual match closes the exception';
    END IF;
    IF (NEW.accepted_by, NEW.accepted_at) IS DISTINCT FROM (OLD.accepted_by, OLD.accepted_at)
        AND NEW.status <> 'ACCEPTED_RISK' THEN
        RAISE EXCEPTION 'acceptance is recorded only on entering ACCEPTED_RISK';
    END IF;

    IF NEW.status = 'CLOSED' THEN
        NEW.resolved_at := COALESCE(NEW.resolved_at, NOW());
    END IF;
    IF NEW.status = 'ACCEPTED_RISK' THEN
        NEW.accepted_at := COALESCE(NEW.accepted_at, NOW());
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE IF NOT EXISTS reconciliation_exception_transitions (
    transition_id   BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       VARCHAR(64)  NOT NULL,
    exception_id    TEXT         NOT NULL REFERENCES reconciliation_exceptions(exception_id),
    from_state      VARCHAR(16)  NOT NULL,
    to_state        VARCHAR(16)  NOT NULL,
    owner_principal VARCHAR(255),
    reason          TEXT         NOT NULL,
    authority_ref   VARCHAR(500),
    evidence_ref    VARCHAR(500),
    root_cause_code VARCHAR(48),
    root_cause_note TEXT,
    actor           VARCHAR(255) NOT NULL,
    occurred_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_reconciliation_exception_transitions_exception
    ON reconciliation_exception_transitions (tenant_id, exception_id, transition_id);

ALTER TABLE reconciliation_exception_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_exception_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY reconciliation_exception_transitions_tenant_isolation ON reconciliation_exception_transitions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE TRIGGER trigger_exception_transitions_immutable
    BEFORE UPDATE OR DELETE ON reconciliation_exception_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

-- ── Certifications ──────────────────────────────────────────────────────────

ALTER TABLE certifications
    ADD COLUMN result_state VARCHAR(16) CHECK (result_state IN ('MATCH', 'VARIANCE')),
    ADD COLUMN assessment   JSONB;

ALTER TABLE certifications
    ADD CONSTRAINT certifications_assessment_check CHECK (
        result_state IS NOT NULL AND jsonb_typeof(assessment) = 'object'
    ) NOT VALID;
