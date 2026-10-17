-- Migration: 000004_integrity_score_threshold.up.sql
--
-- Acceptance threshold (SPEC_DEVIATIONS A11). The 0-100 integrity score had
-- no pass/fail semantics: a job was FAILED only when every record was
-- invalid, so 999 invalid records out of 1000 read as COMPLETED.
--
-- The threshold a job was accepted against is stored per job rather than
-- only kept in service configuration: when the configured default changes,
-- a job decided earlier must still say which bar it actually cleared (or
-- missed). The application writes the threshold it actually applied on
-- every insert; the DEFAULT here backfills existing rows with the service
-- default (95.00) that applies from this migration on.

ALTER TABLE migration_jobs
    ADD COLUMN IF NOT EXISTS integrity_score_threshold NUMERIC(5,2) NOT NULL DEFAULT 95.00;
