-- Migration: 000028_ledger_entries_date_window_index.up.sql
--
-- QueryLedger's transaction-date window (transaction_date_from/_to) on one
-- account. reconciliation-intelligence-svc proposes the book side of an
-- UNMATCHED bank statement line by reading one cash account's entries in a
-- date window around the statement date. idx_ledger_entries_scope cannot
-- serve that: book_id sits between legal_entity_id and account_code, and it
-- carries no date, so the read would scan every entry of the entity.
--
-- Index only: no column, no row and no constraint changes.

CREATE INDEX IF NOT EXISTS idx_ledger_entries_account_txn_date
    ON ledger_entries (tenant_id, legal_entity_id, account_code, transaction_date);
