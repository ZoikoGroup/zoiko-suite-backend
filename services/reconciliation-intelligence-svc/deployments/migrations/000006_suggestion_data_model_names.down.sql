-- Migration: 000006_suggestion_data_model_names.down.sql
--
-- Manual rollback only. This restores the pre-A9 names; it cannot preserve
-- the suggestion-level legal_entity_id, because the old model had no column
-- for it.

DROP INDEX IF EXISTS idx_reconciliation_suggestion_legal_entity;

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS legal_entity_id;

ALTER INDEX idx_reconciliation_suggestion_status
    RENAME TO idx_reconciliation_unmatched_status;

ALTER TABLE reconciliation_unmatched_items
    RENAME COLUMN suggestion_status TO resolution_status;

ALTER TABLE reconciliation_unmatched_items
    RENAME COLUMN source_record_b TO transaction_ref_b;

ALTER TABLE reconciliation_unmatched_items
    RENAME COLUMN source_record_a TO transaction_ref_a;
