DROP TRIGGER IF EXISTS trg_reject_cash_position_mutation ON cash_position_snapshots;
DROP FUNCTION IF EXISTS reject_cash_position_mutation();
DROP TABLE IF EXISTS cash_position_snapshots;
