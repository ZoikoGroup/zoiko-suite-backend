-- Reverting is possible only while no exception is SUPERSEDED: the restored
-- status CHECK does not admit it, and exceptions are append-only history, so
-- those rows cannot be rewritten. Postgres refuses the ADD CONSTRAINT below if
-- any exist, which is the intended outcome.
DROP INDEX IF EXISTS uq_reconciliation_exceptions_balance;
CREATE UNIQUE INDEX uq_reconciliation_exceptions_balance
    ON reconciliation_exceptions (run_id, currency, dimension, dimension_value) WHERE kind = 'BALANCE';

ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_supersession_check;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_status_check;
ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_status_check
        CHECK (status IN ('OPEN', 'INVESTIGATING', 'REMEDIATED', 'ACCEPTED_RISK', 'CLOSED'));
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_balance_has_generation;

-- The lifecycle function as 000007 defined it.
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

ALTER TABLE reconciliation_exceptions
    DROP COLUMN IF EXISTS superseded_reason, DROP COLUMN IF EXISTS superseded_by_generation,
    DROP COLUMN IF EXISTS superseded_by, DROP COLUMN IF EXISTS superseded_at, DROP COLUMN IF EXISTS raised_generation;
