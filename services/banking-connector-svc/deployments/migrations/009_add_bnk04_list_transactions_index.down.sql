-- No refusal guard, deliberately: the up migration adds only an index, which
-- holds no data. Dropping it loses nothing a re-run of the up cannot rebuild;
-- ListTransactions stays correct, only slower.

DROP INDEX IF EXISTS idx_bank_txn_canonical_tenant_status_date;
