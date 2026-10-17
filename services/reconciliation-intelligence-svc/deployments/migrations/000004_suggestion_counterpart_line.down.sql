-- Migration: 000004_suggestion_counterpart_line.down.sql
--
-- Manual rollback only. DUPLICATE_ENTRY suggestions lose the line they name
-- as duplicated (it remains in their resolution_notes).

DROP INDEX IF EXISTS idx_reconciliation_unmatched_counterpart_line;

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS counterpart_statement_line_id;
