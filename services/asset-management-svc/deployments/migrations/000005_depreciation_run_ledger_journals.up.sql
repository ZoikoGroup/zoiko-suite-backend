-- A depreciation run posted through the general ledger accounting kernel produces one journal per asset category (the kernel reconciles the
-- asset register by class), so a run can own several ledger journals. journal_id keeps the first one for everything that reads it;
-- ledger_journal_ids lists all of them, and supersede reverses every one. A run posted through the legacy command has an empty list.
ALTER TABLE depreciation_runs ADD COLUMN ledger_journal_ids JSONB NOT NULL DEFAULT '[]'::jsonb;
