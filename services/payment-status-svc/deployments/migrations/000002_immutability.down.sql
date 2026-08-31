DROP TRIGGER IF EXISTS trg_reject_status_events_mutation ON status_events;
DROP TRIGGER IF EXISTS trg_reject_execution_state_mutation ON payment_execution_states;
DROP FUNCTION IF EXISTS reject_evidence_mutation();
DROP FUNCTION IF EXISTS reject_execution_state_mutation();
