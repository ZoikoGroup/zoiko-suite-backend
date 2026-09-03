DROP TABLE IF EXISTS trial_balance_lines;
DROP TABLE IF EXISTS trial_balance_snapshots;
DROP FUNCTION IF EXISTS reject_trial_balance_mutation();
DROP INDEX IF EXISTS idx_journal_headers_journal_seq;
ALTER TABLE journal_headers DROP COLUMN IF EXISTS journal_seq;
