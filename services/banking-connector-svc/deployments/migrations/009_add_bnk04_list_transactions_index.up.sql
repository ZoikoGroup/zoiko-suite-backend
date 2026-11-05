-- BNK-04 ListTransactions (GET /v1/banking/canonical-transactions) reads one
-- tenant's NORMALIZED canonical transactions in a transaction_date window.
-- idx_bank_txn_canonical_tenant_status (004) stops at status, so the window
-- would scan every NORMALIZED row the tenant has ever had.
--
-- Index only: no column, row or constraint changes.

CREATE INDEX IF NOT EXISTS idx_bank_txn_canonical_tenant_status_date
    ON bank_transactions_canonical (tenant_id, status, transaction_date);
