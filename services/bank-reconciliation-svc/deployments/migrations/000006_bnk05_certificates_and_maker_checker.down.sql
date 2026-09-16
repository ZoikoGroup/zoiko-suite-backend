DROP TABLE IF EXISTS reconciliation_certificates CASCADE;

ALTER TABLE statement_lines
    DROP COLUMN IF EXISTS proposed_journal_id,
    DROP COLUMN IF EXISTS proposed_by_principal_id,
    DROP COLUMN IF EXISTS proposed_at;
