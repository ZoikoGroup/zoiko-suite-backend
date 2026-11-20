DROP TRIGGER IF EXISTS trg_reject_identifier_token_mutation ON bank_account_identifier_tokens;
DROP FUNCTION IF EXISTS reject_identifier_token_mutation();
DROP TABLE IF EXISTS bank_account_identifier_tokens;
