ALTER TABLE journal_headers DROP CONSTRAINT journal_headers_purpose;
ALTER TABLE journal_headers ADD CONSTRAINT journal_headers_purpose CHECK (journal_purpose IS NULL OR journal_purpose IN
    ('ACCRUAL', 'PREPAYMENT', 'RECLASSIFICATION', 'CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'OPENING_BALANCE', 'MIGRATION', 'CLOSE_ADJUSTMENT',
     'ALLOCATION', 'REVALUATION', 'RECONCILIATION_ADJUSTMENT', 'OTHER'));
ALTER TABLE balance_snapshots DROP CONSTRAINT balance_snapshots_purpose;
ALTER TABLE balance_snapshots ADD CONSTRAINT balance_snapshots_purpose CHECK (purpose IN ('CLOSE', 'RECLOSE', 'CONSOLIDATION_FREEZE', 'AD_HOC'));
DROP TRIGGER IF EXISTS consolidation_run_guard_trg ON consolidation_runs;
DROP FUNCTION IF EXISTS consolidation_run_guard();
DROP TABLE IF EXISTS consolidation_runs;
DROP TRIGGER IF EXISTS consolidation_mapping_guard_trg ON consolidation_mappings;
DROP FUNCTION IF EXISTS consolidation_mapping_guard();
DROP TABLE IF EXISTS consolidation_mappings;
DROP TRIGGER IF EXISTS consolidation_member_guard_trg ON consolidation_members;
DROP FUNCTION IF EXISTS consolidation_member_guard();
DROP TABLE IF EXISTS consolidation_members;
DROP TABLE IF EXISTS consolidation_groups;
