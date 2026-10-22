-- Migration: 000005_suggestion_score_provenance.up.sql
--
-- A8: every suggestion now carries its score basis instead of only a
-- fixed confidence number plus prose. heuristic_rule names the fixed rule
-- that fired (MISSING_REFERENCE_WRITE_OFF, MISSING_REFERENCE_MANUAL_REVIEW,
-- DUPLICATE_EXACT or DUPLICATE_SUFFIXED), and tolerance_version records the
-- financial-control-svc TolerancePolicy version that was in force for the
-- analysis. The version used to be fetched and discarded.
--
-- Both columns are NULL on suggestions written before this migration:
-- those values were never recorded, so there is nothing true to backfill.

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN IF NOT EXISTS heuristic_rule VARCHAR(64),
    ADD COLUMN IF NOT EXISTS tolerance_version INT;
