ALTER TABLE journal_headers DROP CONSTRAINT IF EXISTS journal_headers_purpose;
ALTER TABLE journal_headers ADD CONSTRAINT journal_headers_purpose CHECK (journal_purpose IS NULL OR journal_purpose IN
    ('ACCRUAL', 'PREPAYMENT', 'RECLASSIFICATION', 'CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'OPENING_BALANCE', 'MIGRATION', 'CLOSE_ADJUSTMENT', 'ALLOCATION', 'REVALUATION', 'OTHER'));
DROP INDEX IF EXISTS idx_journal_headers_recon;
ALTER TABLE journal_headers DROP COLUMN IF EXISTS reconciliation_case_id;
DROP TABLE IF EXISTS reconciliation_case_events;
DROP TRIGGER IF EXISTS reconciliation_runs_guard ON reconciliation_runs;
DROP FUNCTION IF EXISTS reconciliation_run_guard();
DROP TABLE IF EXISTS reconciliation_runs;
DROP TABLE IF EXISTS reconciliation_tolerances;
DROP INDEX IF EXISTS idx_ledger_entries_counterparty;
DROP INDEX IF EXISTS idx_ledger_entries_control;
ALTER TABLE ledger_entries DROP COLUMN IF EXISTS counterparty_id, DROP COLUMN IF EXISTS control_type;
ALTER TABLE journal_lines DROP COLUMN IF EXISTS counterparty_id;
ALTER TABLE chart_of_accounts DROP CONSTRAINT IF EXISTS chart_control_type, DROP COLUMN IF EXISTS control_type, DROP COLUMN IF EXISTS counterparty_required;
