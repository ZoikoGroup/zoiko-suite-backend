-- Reverses 000010_add_journal_approval_lifecycle.up.sql.
--
-- Dropping these columns discards every journal's approval workflow
-- history (who submitted/approved/rejected/requested posting, when, and
-- what was approved). Safe to run only on an environment whose ledger is
-- disposable.

DROP INDEX IF EXISTS idx_journal_headers_correction_of;
DROP INDEX IF EXISTS idx_journal_headers_approval_status;

ALTER TABLE journal_headers
    DROP COLUMN IF EXISTS correction_of_journal_id,
    DROP COLUMN IF EXISTS posting_requested_by_principal_id,
    DROP COLUMN IF EXISTS posting_requested_at,
    DROP COLUMN IF EXISTS rejection_reason,
    DROP COLUMN IF EXISTS rejected_by_principal_id,
    DROP COLUMN IF EXISTS rejected_at,
    DROP COLUMN IF EXISTS approved_by_principal_id,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS submitted_by_principal_id,
    DROP COLUMN IF EXISTS submitted_at,
    DROP COLUMN IF EXISTS approval_fingerprint,
    DROP COLUMN IF EXISTS approval_status;
