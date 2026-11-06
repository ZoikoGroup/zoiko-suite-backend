ALTER TABLE asset_events
    DROP COLUMN credit_mapping_key,
    DROP COLUMN debit_mapping_key;

ALTER TABLE depreciation_runs
    DROP COLUMN accumulated_depreciation_mapping_key,
    DROP COLUMN depreciation_expense_mapping_key;
