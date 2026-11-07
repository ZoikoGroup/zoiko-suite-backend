-- Global Accounting Kernel (standard §27.1): the ledger can be reconciled against a signed snapshot (the trial balance a report is
-- made from), next to the intercompany reciprocal balances the run type list already allowed.
ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_type;
ALTER TABLE reconciliation_runs ADD CONSTRAINT reconciliation_runs_type CHECK (reconciliation_type IN
    ('AR_GL', 'AP_GL', 'BANK_GL', 'PAYROLL_GL', 'TAX_GL', 'ASSET_GL', 'INVENTORY_GL', 'INTERCOMPANY_GL', 'LEDGER_SNAPSHOT', 'EVENT_POSTING'));
