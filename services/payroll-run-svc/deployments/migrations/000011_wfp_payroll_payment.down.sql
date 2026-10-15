ALTER TABLE wfp_liability_settlements DROP COLUMN IF EXISTS instruction_id;
DROP TABLE IF EXISTS wfp_payment_approvals;
DROP TABLE IF EXISTS wfp_payment_instructions;
DROP FUNCTION IF EXISTS wfp_pa_guard();
DROP FUNCTION IF EXISTS wfp_pi_update_guard();
DROP FUNCTION IF EXISTS wfp_pi_insert_guard();
