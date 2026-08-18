-- Migration: 000003_add_gl_cash_account_code.up.sql
--
-- Records which general-ledger account represents the bank account a
-- statement line was drawn from, so a match can verify DIRECTION.
--
-- Why this column has to exist for the service to do its job:
--
-- Matching previously compared magnitudes — abs(journal net) against
-- abs(statement amount). A journal's "net amount" was the sum of one side of
-- a balanced double-entry journal, which is always positive and therefore
-- carries no direction at all. So a statement line of -500.00 (money leaving
-- the bank account) reconciled cleanly against a journal that moved 500.00
-- IN, and vice versa. Direction is the property a bank reconciliation exists
-- to assert; a payment out matching a receipt in is exactly the error, or the
-- concealment, that reconciling is supposed to surface.
--
-- Direction cannot be recovered from the journal alone. It depends on which
-- side of the journal the CASH line falls on: a debit to the bank's own
-- ledger account is money in, a credit is money out. That requires knowing
-- which account code IS the bank account — which is what this column records.
--
-- Nullable, because rows ingested before this migration have no value that
-- could be honestly backfilled: nothing in this service, or anywhere else in
-- the platform, maps a bank_account_id to a ledger account code. A NULL is
-- not treated as "skip the check" — internal/handler refuses to match such a
-- line at all, with a distinct error, rather than falling back to the
-- magnitude comparison this migration exists to retire. Verifying a weaker
-- property and reporting it as a match is what was wrong before.
ALTER TABLE statement_lines
    ADD COLUMN gl_cash_account_code VARCHAR(50);

COMMENT ON COLUMN statement_lines.gl_cash_account_code IS
    'general-ledger account code representing this bank account. Required to verify the direction of a match; a line without one cannot be matched.';
