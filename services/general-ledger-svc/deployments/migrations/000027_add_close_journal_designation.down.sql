-- Migration: 000027_add_close_journal_designation.down.sql
--
-- Refused while any journal carries a close-journal designation. The
-- designation is the only record that a posting was admitted to CLOSE_REVIEW
-- as a named close workflow rather than as an ordinary journal; dropping the
-- column would erase that evidence while the journal itself remains in the
-- ledger. Same posture as the other irreplaceable-data down migrations in
-- this domain.

-- The check must see every tenant's rows, not only the migration role's
-- tenant scope. journal_headers is FORCE ROW LEVEL SECURITY (000004), so an
-- ordinary owner would otherwise read zero rows and silently drop the column.
-- A role that cannot disable row security fails the query instead, which is
-- still a refusal rather than a loss of evidence.
SET LOCAL row_security = off;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM journal_headers WHERE close_journal_designation IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 000027: at least one journal carries a close_journal_designation';
    END IF;
END $$;

ALTER TABLE journal_headers
    DROP CONSTRAINT IF EXISTS journal_headers_close_journal_designation_check;

ALTER TABLE journal_headers
    DROP COLUMN IF EXISTS close_journal_designation;
