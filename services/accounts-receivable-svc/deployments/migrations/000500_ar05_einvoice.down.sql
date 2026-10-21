-- Migration: 000500_ar05_einvoice.down.sql
DROP TABLE IF EXISTS ar05_security_events;
DROP TABLE IF EXISTS ar05_submission_history;
DROP TABLE IF EXISTS ar05_reconciliations;
DROP TABLE IF EXISTS ar05_callback_nonces;
DROP TABLE IF EXISTS ar05_acknowledgements;
DROP TABLE IF EXISTS ar05_attempt_index;
DROP TABLE IF EXISTS ar05_attempt_results;
DROP TABLE IF EXISTS ar05_submission_attempts;
DROP TABLE IF EXISTS ar05_validation_results;
DROP TABLE IF EXISTS ar05_artifacts;
DROP TABLE IF EXISTS ar05_einvoice_submissions;
DROP FUNCTION IF EXISTS ar05_guard_submission();
