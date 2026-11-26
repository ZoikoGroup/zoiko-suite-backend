-- Phase 2 of tokenization enforcement (SPEC_DEVIATIONS.md B12): once a row
-- has a verified token in payee_destination_identifier_tokens,
-- cmd/scrub-legacy-raw nulls payee_destinations.account_identifier. The
-- column was NOT NULL, which would refuse that write outright, so this
-- loosens it to nullable.
--
-- This does NOT drop the column (it stays, NULL, as the pre-existing-raw
-- marker for rows scrub never touches) and does NOT change any default.
ALTER TABLE payee_destinations ALTER COLUMN account_identifier DROP NOT NULL;
