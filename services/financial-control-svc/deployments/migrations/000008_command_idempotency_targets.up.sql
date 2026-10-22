-- Migration: 000008_command_idempotency_targets.up.sql
--
-- SPEC_DEVIATIONS A3. ZS-CONTROL-001's command table: "Idempotency: Mandatory
-- for run creation, execution, reperformance and certification commands."
-- Certification (POST .../certification), resubmission
-- (POST .../submit-for-certification) and exception transitions (including
-- REPERFORMED) now claim their Idempotency-Key in command_idempotency, the
-- table ExecuteRun already uses — one mechanism, not a second one.
--
-- Two widenings, nothing else changes:
--   * exception_id: an exception transition acts on one exception, not on
--     the run, so the claim must name it. Without it, the same key retried
--     against two exceptions of one run would read as a replay of the first.
--   * command VARCHAR(64): commands now carry what they decide ("CERTIFY" vs
--     "REJECT", "EXCEPTION:<to_state>"), so one key cannot replay a different
--     decision; the longest, EXCEPTION:CARRIED_FORWARD_UNDER_AUTHORITY, is 41.

ALTER TABLE command_idempotency
    ALTER COLUMN command TYPE VARCHAR(64),
    ADD COLUMN exception_id UUID REFERENCES control_exceptions(exception_id);
