-- Reverses 000007. Dropping the token table makes every TOKENIZED destination
-- unreadable as an account identifier: payee_destinations.account_identifier
-- holds the token itself (not the raw value) for destinations proposed with
-- TOKENIZATION_ENFORCED=true. Only run this before any such destination exists.
DROP TABLE IF EXISTS payee_destination_identifier_tokens;
