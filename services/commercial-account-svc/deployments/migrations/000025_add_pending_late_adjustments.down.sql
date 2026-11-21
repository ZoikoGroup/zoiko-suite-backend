-- 000025_add_pending_late_adjustments.down.sql
-- Manual rollback only, and refused while any pending late adjustment is
-- recorded -- same posture as every other durable-queue/evidence down
-- migration in this service (e.g. 000024's entitlement_invalidation_queue
-- has no down at all for the same reason; this one is reversible only
-- because the table is new and brand-new systems have nothing in it yet).
-- Dropping the table while rows exist would silently forget a late fact
-- this migration exists to never lose.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pending_late_adjustments) THEN
        RAISE EXCEPTION 'cannot roll back 000025: pending late adjustments exist';
    END IF;
END $$;

DROP TRIGGER IF EXISTS trg_pending_late_adjustments_lifecycle ON pending_late_adjustments;
DROP FUNCTION IF EXISTS enforce_pending_late_adjustment_lifecycle();
DROP TABLE IF EXISTS pending_late_adjustments;
