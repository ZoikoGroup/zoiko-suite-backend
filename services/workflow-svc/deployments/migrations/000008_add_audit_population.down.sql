DROP TRIGGER IF EXISTS trg_reject_frozen_population_manifest_mutation ON audit_populations;
DROP FUNCTION IF EXISTS reject_frozen_population_manifest_mutation();
DROP TRIGGER IF EXISTS trg_reject_audit_population_transition_mutation ON audit_population_transitions;
DROP FUNCTION IF EXISTS reject_audit_population_transition_mutation();
DROP TABLE IF EXISTS audit_population_transitions;
DROP TABLE IF EXISTS audit_populations;
