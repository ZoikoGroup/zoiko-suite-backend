-- Migration: 000025_subledger_control_governance.down.sql

DROP TABLE IF EXISTS control_exceptions;

ALTER TABLE subledger_control_runs
    DROP CONSTRAINT IF EXISTS subledger_control_runs_certification_status_check,
    DROP COLUMN IF EXISTS certification_status,
    DROP COLUMN IF EXISTS certified_at,
    DROP COLUMN IF EXISTS certified_by_principal_id,
    DROP COLUMN IF EXISTS certification_note;

-- Restore migration 000004's strict append-only guard: with the
-- certification columns gone there is no legitimate update left.
CREATE OR REPLACE FUNCTION reject_control_run_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'subledger_control_runs is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;
