DROP TRIGGER IF EXISTS trg_reject_settlement_application_mutation ON settlement_applications;
DROP FUNCTION IF EXISTS reject_settlement_application_mutation();
DROP TRIGGER IF EXISTS trg_reject_payable_mutation ON payable_open_items;
DROP FUNCTION IF EXISTS reject_payable_mutation();
