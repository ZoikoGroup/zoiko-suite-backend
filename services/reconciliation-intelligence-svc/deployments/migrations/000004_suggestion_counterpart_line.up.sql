-- A DUPLICATE_ENTRY suggestion is about two bank-reconciliation statement
-- lines: statement_line_id (the probable copy) and this one (the line it
-- duplicates). NULL on every single-line suggestion.

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN IF NOT EXISTS counterpart_statement_line_id UUID;

-- "Which suggestions mention this statement line?" has to look at both
-- columns now; this indexes the second.
CREATE INDEX IF NOT EXISTS idx_reconciliation_unmatched_counterpart_line
    ON reconciliation_unmatched_items (tenant_id, counterpart_statement_line_id)
    WHERE counterpart_statement_line_id IS NOT NULL;
