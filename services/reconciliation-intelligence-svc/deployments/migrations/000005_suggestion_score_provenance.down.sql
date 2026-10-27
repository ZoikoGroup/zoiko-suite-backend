-- Migration: 000005_suggestion_score_provenance.down.sql
--
-- Manual rollback only. Dropping these columns discards the only
-- machine-readable record of which rule produced each suggestion and which
-- tolerance-policy version it was evaluated against.

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS tolerance_version,
    DROP COLUMN IF EXISTS heuristic_rule;
