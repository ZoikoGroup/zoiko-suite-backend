ALTER TABLE import_items DROP COLUMN IF EXISTS release_generation;
-- Reverting drops the quarantine ledger: the durable history of who quarantined, requested,
-- released and re-validated each item is lost. The item rows keep their current status.
DROP TRIGGER IF EXISTS trg_import_quarantine_ledger_immutable ON import_quarantine_ledger;
DROP TRIGGER IF EXISTS trg_import_quarantine_ledger_guard ON import_quarantine_ledger;
DROP FUNCTION IF EXISTS import_quarantine_ledger_immutable();
DROP FUNCTION IF EXISTS import_quarantine_ledger_guard();
DROP TABLE IF EXISTS import_quarantine_ledger;
