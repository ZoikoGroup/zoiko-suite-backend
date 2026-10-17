-- Migration: 000004_integrity_score_threshold.down.sql
--
-- Reverts the stored acceptance bar. Jobs keep their status; what is lost
-- is the bar each was judged against.

ALTER TABLE migration_jobs
    DROP COLUMN IF EXISTS integrity_score_threshold;
