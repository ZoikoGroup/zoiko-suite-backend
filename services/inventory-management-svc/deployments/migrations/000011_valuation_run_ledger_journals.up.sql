-- A valuation run posted through the general ledger accounting kernel produces one journal per location and item type per posting (cost of
-- sales, the goods-received reclass), so a run can own several ledger journals. journal_id keeps the first for everything that reads it;
-- ledger_journal_ids lists all of them. A run posted through the legacy command has an empty list.
ALTER TABLE inventory_valuation_runs ADD COLUMN ledger_journal_ids JSONB NOT NULL DEFAULT '[]'::jsonb;
