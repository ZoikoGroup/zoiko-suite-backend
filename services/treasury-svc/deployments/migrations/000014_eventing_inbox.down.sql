-- Dropping the inbox discards its receipts, so a redelivered
-- BankStatementAccepted event would be reprocessed. Applying it twice is
-- safe but not free (it writes a second cash_balances row and a second
-- cash-position recalculation) — acceptable for a down-migration, not for
-- steady-state operation.
DROP TABLE IF EXISTS eventing_inbox;
