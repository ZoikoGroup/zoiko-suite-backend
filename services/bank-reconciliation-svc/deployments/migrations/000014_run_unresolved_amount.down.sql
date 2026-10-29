ALTER TABLE reconciliation_runs
    DROP COLUMN IF EXISTS max_unresolved_amount,
    DROP COLUMN IF EXISTS policy_currency;
