-- Migration: 000006_suggestion_data_model_names.up.sql
--
-- A9: align the suggestion rows with the canonical data model's
-- ReconciliationSuggestion entity (04-data-model.md §16):
--   transaction_ref_a/b -> source_record_a/b
--   resolution_status   -> suggestion_status
-- and put legal_entity_id on the suggestion itself, not only on its job.
--
-- Existing rows get their legal entity from their job. The relationship is
-- NOT NULL (job_id references reconciliation_jobs), so every row can be
-- backfilled truthfully; no default or guessed entity is used.

ALTER TABLE reconciliation_unmatched_items
    RENAME COLUMN transaction_ref_a TO source_record_a;

ALTER TABLE reconciliation_unmatched_items
    RENAME COLUMN transaction_ref_b TO source_record_b;

ALTER TABLE reconciliation_unmatched_items
    RENAME COLUMN resolution_status TO suggestion_status;

ALTER INDEX idx_reconciliation_unmatched_status
    RENAME TO idx_reconciliation_suggestion_status;

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN IF NOT EXISTS legal_entity_id VARCHAR(64);

UPDATE reconciliation_unmatched_items AS i
SET legal_entity_id = j.legal_entity_id
FROM reconciliation_jobs AS j
WHERE i.job_id = j.id
  AND i.legal_entity_id IS NULL;

ALTER TABLE reconciliation_unmatched_items
    ALTER COLUMN legal_entity_id SET NOT NULL;

-- "This entity's suggestions" is a read path now that the entity is on the
-- row, mirroring the jobs index.
CREATE INDEX IF NOT EXISTS idx_reconciliation_suggestion_legal_entity
    ON reconciliation_unmatched_items (tenant_id, legal_entity_id);
