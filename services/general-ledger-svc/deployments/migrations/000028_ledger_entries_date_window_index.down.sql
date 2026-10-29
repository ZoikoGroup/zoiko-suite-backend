-- Migration: 000028_ledger_entries_date_window_index.down.sql
--
-- No refusal guard, deliberately: the up migration adds only an index, which
-- holds no data of its own. Dropping it loses nothing that cannot be rebuilt
-- by re-running the up migration; the date-window read stays correct, only
-- slower. Compare a down migration that drops a data-bearing column, which
-- must refuse while any row carries a value.

DROP INDEX IF EXISTS idx_ledger_entries_account_txn_date;
