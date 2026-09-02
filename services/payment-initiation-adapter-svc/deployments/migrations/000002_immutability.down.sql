DROP TRIGGER IF EXISTS trg_reject_attempt_events_mutation ON attempt_events;
DROP TRIGGER IF EXISTS trg_reject_attempt_mutation ON payment_initiation_attempts;
DROP FUNCTION IF EXISTS reject_evidence_mutation();
DROP FUNCTION IF EXISTS reject_attempt_mutation();
