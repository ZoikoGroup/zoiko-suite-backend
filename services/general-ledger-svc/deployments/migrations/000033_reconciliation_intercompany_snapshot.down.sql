ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_type;
ALTER TABLE reconciliation_runs ADD CONSTRAINT reconciliation_runs_type CHECK (reconciliation_type IN
    ('AR_GL', 'AP_GL', 'BANK_GL', 'PAYROLL_GL', 'TAX_GL', 'ASSET_GL', 'INVENTORY_GL', 'INTERCOMPANY_GL', 'EVENT_POSTING'));
