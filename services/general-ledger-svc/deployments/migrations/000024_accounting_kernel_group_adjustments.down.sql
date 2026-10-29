ALTER TABLE journal_headers DROP CONSTRAINT journal_headers_purpose;
ALTER TABLE journal_headers ADD CONSTRAINT journal_headers_purpose CHECK (journal_purpose IS NULL OR journal_purpose IN
    ('ACCRUAL', 'PREPAYMENT', 'RECLASSIFICATION', 'CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'OPENING_BALANCE', 'MIGRATION', 'CLOSE_ADJUSTMENT',
     'ALLOCATION', 'REVALUATION', 'RECONCILIATION_ADJUSTMENT', 'CONSOLIDATION', 'ELIMINATION', 'OTHER'));
ALTER TABLE consolidation_runs DROP COLUMN IF EXISTS adjustment_journal_id;
DROP TRIGGER IF EXISTS consolidation_adjustment_guard_trg ON consolidation_adjustments;
DROP FUNCTION IF EXISTS consolidation_adjustment_guard();
DROP TABLE IF EXISTS consolidation_adjustments;
ALTER TABLE consolidation_members DROP CONSTRAINT IF EXISTS consolidation_members_capital,
    DROP COLUMN IF EXISTS investment_account, DROP COLUMN IF EXISTS investor_entity_id, DROP COLUMN IF EXISTS equity_accounts;
