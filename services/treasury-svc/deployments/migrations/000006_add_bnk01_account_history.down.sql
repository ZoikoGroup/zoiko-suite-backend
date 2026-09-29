DROP TRIGGER IF EXISTS trg_reject_account_history_mutation ON bank_account_history;
DROP FUNCTION IF EXISTS reject_account_history_mutation();
DROP TABLE IF EXISTS bank_account_history;
