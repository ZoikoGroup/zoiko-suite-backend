-- ZS-SVC-Z-001 DQC-05 "Re-test" / INV-23 (docs/architecture/DQC_GAP_MATRIX.md, step D).
--
-- THE DEFECT THIS CLOSES. Reperform re-reads a run's populations and re-tests it
-- as a new snapshot generation, with earlier generations kept immutable beside
-- it. But nothing could ever clear an exception raised under an EARLIER
-- generation: CLOSED required a manual match (and an item that no longer exists
-- cannot be matched; a balance exception has no item at all), and REMEDIATED
-- still counts as working. So after the owning domain corrected its data and the
-- run was reperformed over populations that now reconcile, the old exceptions
-- stayed OPEN and Certify refused: the only ways out were to ACCEPT RISK on a
-- variance that no longer exists, or abandon the run. Verified empirically
-- before this migration was written: both an item exception and a balance
-- exception survived a corrected, balanced reperform.
--
-- Separately, a balance exception was unique per (run, currency, bucket) with
-- ON CONFLICT DO NOTHING, so a re-test that found the SAME bucket with a
-- DIFFERENT variance was silently dropped, leaving the old numbers standing as
-- the run's evidence.
--
-- THE FIX. A new terminal status, SUPERSEDED: "a later evidence run replaced
-- this finding". Reperform moves every still-working exception raised under an
-- earlier generation to it, recording who, when, by which generation and why;
-- the new generation's own matching then defines the live exception set. Nothing
-- is deleted, ACCEPTED_RISK and CLOSED exceptions are untouched (acceptance is
-- final; INV-19), and a superseded exception is as immutable as a closed one.
-- A balance exception now records the generation it was raised under, so a
-- changed variance is a new exception beside the superseded old one.

-- ── Columns ─────────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_exceptions
    ADD COLUMN IF NOT EXISTS raised_generation        INT,
    ADD COLUMN IF NOT EXISTS superseded_at            TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS superseded_by            VARCHAR(255),
    ADD COLUMN IF NOT EXISTS superseded_by_generation INT,
    ADD COLUMN IF NOT EXISTS superseded_reason        TEXT;

-- An item exception's generation is derivable from its item's snapshot, so only
-- BALANCE exceptions (which have no item) carry it. Existing balance exceptions
-- are backfilled with the generation that was current when they were raised: the
-- highest generation whose snapshot already existed at raised_at (1 if none is
-- earlier). The lifecycle trigger forbids any UPDATE of a terminal exception, so
-- it is paused for exactly this statement, the same precedent migration 000007
-- set for its own status rewrite, and restored immediately.
ALTER TABLE reconciliation_exceptions DISABLE TRIGGER trigger_exception_resolution;
UPDATE reconciliation_exceptions e
   SET raised_generation = COALESCE(
        (SELECT MAX(ps.generation) FROM population_snapshots ps
          WHERE ps.run_id = e.run_id AND ps.created_at <= e.raised_at), 1)
 WHERE e.kind = 'BALANCE' AND e.raised_generation IS NULL;
ALTER TABLE reconciliation_exceptions ENABLE TRIGGER trigger_exception_resolution;

ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_balance_has_generation
        CHECK (kind <> 'BALANCE' OR raised_generation >= 1);

-- ── Status ──────────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_status_check;
ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_status_check
        CHECK (status IN ('OPEN', 'INVESTIGATING', 'REMEDIATED', 'ACCEPTED_RISK', 'CLOSED', 'SUPERSEDED'));

-- A superseded exception says who, when, by which generation and why; nothing
-- else carries those fields. Valid for every row.
ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_supersession_check
        CHECK (
            (status = 'SUPERSEDED'
                AND superseded_at IS NOT NULL
                AND superseded_by IS NOT NULL AND btrim(superseded_by) <> ''
                AND superseded_by_generation IS NOT NULL AND superseded_by_generation >= 2
                AND superseded_reason IS NOT NULL AND btrim(superseded_reason) <> '')
            OR
            (status <> 'SUPERSEDED'
                AND superseded_at IS NULL AND superseded_by IS NULL
                AND superseded_by_generation IS NULL AND superseded_reason IS NULL)
        );

-- ── Balance uniqueness is per generation ────────────────────────────────────

DROP INDEX IF EXISTS uq_reconciliation_exceptions_balance;
-- A balance variance is raised once per run, currency, bucket AND generation,
-- however many times matching is re-run within that generation. A re-test that
-- finds the same bucket again, with whatever numbers, is a new generation's
-- exception beside the superseded old one.
CREATE UNIQUE INDEX uq_reconciliation_exceptions_balance
    ON reconciliation_exceptions (run_id, currency, dimension, dimension_value, raised_generation) WHERE kind = 'BALANCE';

-- ── Lifecycle ───────────────────────────────────────────────────────────────

-- As defined in 000007, with SUPERSEDED added: terminal and immutable like CLOSED
-- and ACCEPTED_RISK, reachable only from a working status, with its fields set
-- only on entering it, and raised_generation fixed once raised.
CREATE OR REPLACE FUNCTION enforce_exception_resolution()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation exceptions cannot be deleted';
    END IF;

    IF OLD.status IN ('CLOSED', 'ACCEPTED_RISK', 'SUPERSEDED') THEN
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
        OR NEW.raised_generation IS DISTINCT FROM OLD.raised_generation
    THEN
        RAISE EXCEPTION 'reconciliation exception % may only change its lifecycle, owner and evidence', OLD.exception_id;
    END IF;

    IF NEW.status <> OLD.status THEN
        IF NOT (
            (OLD.status = 'OPEN' AND NEW.status IN ('INVESTIGATING', 'ACCEPTED_RISK', 'CLOSED', 'SUPERSEDED'))
            OR (OLD.status = 'INVESTIGATING' AND NEW.status IN ('REMEDIATED', 'ACCEPTED_RISK', 'CLOSED', 'SUPERSEDED'))
            OR (OLD.status = 'REMEDIATED' AND NEW.status IN ('INVESTIGATING', 'CLOSED', 'SUPERSEDED'))
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
    IF (NEW.superseded_by, NEW.superseded_by_generation, NEW.superseded_reason)
        IS DISTINCT FROM (OLD.superseded_by, OLD.superseded_by_generation, OLD.superseded_reason)
        AND NEW.status <> 'SUPERSEDED' THEN
        RAISE EXCEPTION 'supersession is recorded only on entering SUPERSEDED';
    END IF;

    IF NEW.status = 'CLOSED' THEN
        NEW.resolved_at := COALESCE(NEW.resolved_at, NOW());
    END IF;
    IF NEW.status = 'ACCEPTED_RISK' THEN
        NEW.accepted_at := COALESCE(NEW.accepted_at, NOW());
    END IF;
    IF NEW.status = 'SUPERSEDED' THEN
        NEW.superseded_at := COALESCE(NEW.superseded_at, NOW());
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
