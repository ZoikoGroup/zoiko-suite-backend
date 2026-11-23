-- Phase 2 of tokenization enforcement (SPEC_DEVIATIONS.md G6): once a row has
-- a verified token in bank_connection_account_tokens, cmd/scrub-legacy-raw
-- nulls bank_connections.account_number. The column was NOT NULL, which
-- would refuse that write outright, so this loosens it to nullable.
--
-- This does NOT drop the column (it stays, NULL, as the pre-existing-raw
-- marker for rows scrub never touches) and does NOT change any default.
ALTER TABLE bank_connections ALTER COLUMN account_number DROP NOT NULL;
