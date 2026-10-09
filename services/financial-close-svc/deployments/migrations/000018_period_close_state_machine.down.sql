-- Migration: 000018_period_close_state_machine.down.sql
--
-- Manual rollback only. Close history and reopen requests are lost, and every
-- state the old model cannot express collapses: SOFT_CLOSE and CLOSE_REVIEW
-- become OPEN; HARD_CLOSED and RECLOSED become LOCKED; a period in
-- AUTHORIZED_REOPEN becomes OPEN with no expiry.

DROP TABLE IF EXISTS period_state_transitions;
DROP FUNCTION IF EXISTS reject_period_transition_mutation();
DROP TABLE IF EXISTS period_reopen_requests;
DROP FUNCTION IF EXISTS guard_reopen_request_mutation();
ALTER TABLE fiscal_periods DROP CONSTRAINT IF EXISTS fiscal_periods_reopen_window;
ALTER TABLE fiscal_periods DROP COLUMN IF EXISTS reopen_expires_at;
ALTER TABLE fiscal_periods DROP COLUMN IF EXISTS reopened_at;
ALTER TABLE fiscal_periods DROP CONSTRAINT IF EXISTS fiscal_periods_close_status_known;
UPDATE fiscal_periods SET close_status = 'LOCKED' WHERE close_status IN ('HARD_CLOSED', 'RECLOSED');
UPDATE fiscal_periods SET close_status = 'OPEN' WHERE close_status IN ('SOFT_CLOSE', 'CLOSE_REVIEW', 'AUTHORIZED_REOPEN');
