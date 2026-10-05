DROP TRIGGER IF EXISTS trg_reject_legal_hold_target_mutation ON legal_hold_targets;
DROP FUNCTION IF EXISTS reject_legal_hold_target_mutation();
DROP TABLE IF EXISTS legal_hold_targets CASCADE;

DROP TRIGGER IF EXISTS trg_reject_legal_hold_mutation ON legal_holds;
DROP FUNCTION IF EXISTS reject_legal_hold_mutation();
DROP TABLE IF EXISTS legal_holds CASCADE;

DROP TRIGGER IF EXISTS trg_reject_retention_state_mutation ON record_retention_states;
DROP FUNCTION IF EXISTS reject_retention_state_mutation();
DROP TABLE IF EXISTS record_retention_states CASCADE;

DROP TRIGGER IF EXISTS trg_reject_retention_rule_mutation ON retention_rule_versions;
DROP FUNCTION IF EXISTS reject_retention_rule_mutation();
DROP TABLE IF EXISTS retention_rule_versions CASCADE;
