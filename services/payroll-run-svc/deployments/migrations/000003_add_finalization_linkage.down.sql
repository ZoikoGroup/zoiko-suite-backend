ALTER TABLE payroll_runs
    DROP COLUMN IF EXISTS governance_decision_id,
    DROP COLUMN IF EXISTS snapshot_hash;
