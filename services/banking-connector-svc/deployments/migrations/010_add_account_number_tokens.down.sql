-- Reverses 010. Dropping the token table makes every TOKENIZED connection
-- unreadable as a bank account: bank_connections.account_number holds the
-- token itself (not the raw number) for connections created with
-- TOKENIZATION_ENFORCED=true. Only run this before any such connection exists.
DROP TABLE IF EXISTS bank_connection_account_tokens;
DROP FUNCTION IF EXISTS reject_account_token_mutation();
