-- Migration: 000007_exception_governance_materiality.down.sql
--
-- Restores the 000006 schema. Exception history, an exception in a governed
-- state (INVESTIGATING, REMEDIATED, ACCEPTED_RISK) and a certification over
-- accepted risk have no 000006 representation and are evidence that cannot be
-- deleted, so the down migration refuses while any exists. Definitions and
-- runs written under 000007 have no single materiality; 0 is backfilled (the
-- 000006 column was never read).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_exception_transitions)
        OR EXISTS (SELECT 1 FROM reconciliation_exceptions WHERE status IN ('INVESTIGATING', 'REMEDIATED', 'ACCEPTED_RISK'))
        OR EXISTS (SELECT 1 FROM certifications WHERE result_state = 'VARIANCE')
        OR EXISTS (SELECT 1 FROM reconciliation_runs WHERE result_state = 'VARIANCE' AND certification_state <> 'UNCERTIFIED')
    THEN
        RAISE EXCEPTION 'cannot downgrade below 000007: governed exception state or accepted-risk certification exists and has no 000006 representation';
    END IF;
END $$;

ALTER TABLE certifications DROP CONSTRAINT IF EXISTS certifications_assessment_check;
ALTER TABLE certifications DROP COLUMN IF EXISTS assessment, DROP COLUMN IF EXISTS result_state;

DROP TABLE IF EXISTS reconciliation_exception_transitions;
DROP INDEX IF EXISTS idx_reconciliation_exceptions_owner;

CREATE OR REPLACE FUNCTION enforce_exception_resolution()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation exceptions cannot be deleted';
    END IF;

    IF OLD.status = 'Resolved' THEN
        RAISE EXCEPTION 'reconciliation exception % is already Resolved and immutable', OLD.exception_id;
    END IF;

    IF NEW.status <> 'Resolved' THEN
        RAISE EXCEPTION 'reconciliation exceptions may only transition Open -> Resolved';
    END IF;
    IF NEW.resolved_by_match_id IS NULL THEN
        RAISE EXCEPTION 'resolving an exception requires resolved_by_match_id';
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
    THEN
        RAISE EXCEPTION 'reconciliation exception % may only have status/resolved_at/resolved_by_match_id change', OLD.exception_id;
    END IF;
    NEW.resolved_at := COALESCE(NEW.resolved_at, NOW());
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_resolution_check;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_governance_check;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_status_check;
ALTER TABLE reconciliation_exceptions DISABLE TRIGGER trigger_exception_resolution;
UPDATE reconciliation_exceptions SET status = CASE status WHEN 'OPEN' THEN 'Open' WHEN 'CLOSED' THEN 'Resolved' ELSE status END;
ALTER TABLE reconciliation_exceptions ENABLE TRIGGER trigger_exception_resolution;
ALTER TABLE reconciliation_exceptions
    ALTER COLUMN status SET DEFAULT 'Open',
    ADD CONSTRAINT reconciliation_exceptions_status_check CHECK (status IN ('Open', 'Resolved')),
    DROP COLUMN IF EXISTS accepted_at,
    DROP COLUMN IF EXISTS accepted_by,
    DROP COLUMN IF EXISTS root_cause_note,
    DROP COLUMN IF EXISTS root_cause_code,
    DROP COLUMN IF EXISTS evidence_ref,
    DROP COLUMN IF EXISTS authority_ref,
    DROP COLUMN IF EXISTS due_at,
    DROP COLUMN IF EXISTS owner_principal,
    DROP COLUMN IF EXISTS owner_role,
    DROP COLUMN IF EXISTS exposure_minor_units,
    DROP COLUMN IF EXISTS materiality,
    DROP COLUMN IF EXISTS severity,
    DROP COLUMN IF EXISTS assertion,
    DROP COLUMN IF EXISTS category;

ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_state_dimensions_check;
ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_state_dimensions_check CHECK (
        ((status = 'Certified') = (certification_state = 'CERTIFIED'))
        AND (certification_state NOT IN ('ELIGIBLE', 'CERTIFIED') OR result_state = 'MATCH')
        AND ((status = 'Superseded') = (result_state = 'SUPERSEDED'))
        AND (certification_state <> 'SUPERSEDED' OR status = 'Superseded')
        AND (status <> 'Planned' OR (result_state = 'NOT_RUN' AND certification_state = 'UNCERTIFIED'))
        AND (status <> 'ExceptionsOpen' OR result_state = 'VARIANCE')
        AND (status <> 'Failed' OR (result_state = 'INDETERMINATE' AND failure_code IS NOT NULL))
        AND ((failure_code IS NULL) = (failure_reason IS NULL))
        AND (failure_code IS NULL OR status IN ('Failed', 'Superseded'))
    );
DROP TRIGGER IF EXISTS trigger_run_governance_pinned ON reconciliation_runs;
DROP FUNCTION IF EXISTS enforce_run_governance_pinned();
ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_governance_check;
ALTER TABLE reconciliation_runs DISABLE TRIGGER trigger_run_lifecycle;
UPDATE reconciliation_runs SET materiality_amount_minor_units = 0 WHERE materiality_amount_minor_units IS NULL;
ALTER TABLE reconciliation_runs ENABLE TRIGGER trigger_run_lifecycle;
ALTER TABLE reconciliation_runs
    ALTER COLUMN materiality_amount_minor_units SET NOT NULL,
    DROP COLUMN IF EXISTS certification_profile,
    DROP COLUMN IF EXISTS exception_policy,
    DROP COLUMN IF EXISTS materiality_policy;

ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_governance_check;
ALTER TABLE reconciliation_definitions DISABLE TRIGGER trigger_definition_versioning;
UPDATE reconciliation_definitions SET materiality_amount_minor_units = 0 WHERE materiality_amount_minor_units IS NULL;
ALTER TABLE reconciliation_definitions ENABLE TRIGGER trigger_definition_versioning;
ALTER TABLE reconciliation_definitions
    ALTER COLUMN materiality_amount_minor_units SET NOT NULL,
    DROP COLUMN IF EXISTS certification_profile,
    DROP COLUMN IF EXISTS exception_policy,
    DROP COLUMN IF EXISTS materiality_policy;

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
        NEW.amount_scale, NEW.populations, NEW.tolerance_policy, NEW.balance_dimensions)
       IS DISTINCT FROM
       (OLD.match_key_fields, OLD.tolerance_amount_minor_units, OLD.materiality_amount_minor_units,
        OLD.amount_scale, OLD.populations, OLD.tolerance_policy, OLD.balance_dimensions) THEN
        NEW.version := OLD.version + 1;
        NEW.updated_at := NOW();
    ELSE
        NEW.version := OLD.version;
        NEW.updated_at := OLD.updated_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
