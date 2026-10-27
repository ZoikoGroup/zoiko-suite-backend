-- Migration: 000006_add_consolidation_adjustment_transitions.down.sql
--
-- Manual rollback only, and refused while any transition is recorded. The
-- backfilled rows are reconstructable from consolidation_adjustments' own
-- lifecycle columns (still present, untouched by this migration), but any
-- transition recorded by a live command since carries a reason, details and
-- correlation_id those columns never held — dropping the table would lose
-- that evidence with no way to reconstruct it. Same posture as every other
-- append-only history table this service and its neighbours use.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM consolidation_adjustment_transitions) THEN
        RAISE EXCEPTION 'cannot roll back 000006: adjustment transitions exist';
    END IF;
END $$;

DROP TABLE IF EXISTS consolidation_adjustment_transitions;
