-- 000038 down. The marker column is only bookkeeping for the sweep, so dropping
-- it loses nothing but the record of which expiries were announced; the events
-- themselves stay in the outbox.
DROP FUNCTION IF EXISTS authz_sweep_expired_assignments(INTEGER);
DROP TRIGGER IF EXISTS pra_end_announced ON principal_role_assignments;
DROP FUNCTION IF EXISTS authz_assignment_end_announced();
DROP INDEX IF EXISTS idx_pra_expiry_due;
ALTER TABLE principal_role_assignments DROP COLUMN IF EXISTS expiry_notified_at;
