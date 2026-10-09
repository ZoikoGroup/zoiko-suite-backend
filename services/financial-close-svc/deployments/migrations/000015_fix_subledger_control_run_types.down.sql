-- Migration: 000015_fix_subledger_control_run_types.down.sql
--
-- Manual rollback only. Fails if any run with a subledger longer than ten
-- characters exists — those runs could not be represented in the old column,
-- which is the bug 000015 fixed. Book references on ASSETS runs are lost.

ALTER TABLE subledger_control_runs DROP CONSTRAINT IF EXISTS subledger_control_runs_book_for_assets;
ALTER TABLE subledger_control_runs DROP COLUMN IF EXISTS book_id;
ALTER TABLE subledger_control_runs DROP CONSTRAINT IF EXISTS subledger_control_runs_subledger_known;
ALTER TABLE subledger_control_runs ALTER COLUMN subledger TYPE VARCHAR(10);
