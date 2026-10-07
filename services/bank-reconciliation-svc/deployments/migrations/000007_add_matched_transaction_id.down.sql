ALTER TABLE statement_lines
    DROP COLUMN IF EXISTS matched_transaction_id,
    DROP COLUMN IF EXISTS proposed_transaction_id;
