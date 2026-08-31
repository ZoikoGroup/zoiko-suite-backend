DROP TRIGGER IF EXISTS trg_reject_authorization_events_mutation ON authorization_events;
DROP TRIGGER IF EXISTS trg_reject_authorization_payee_snapshots_mutation ON authorization_payee_snapshots;
DROP TRIGGER IF EXISTS trg_reject_authorization_mutation ON payment_authorizations;
DROP FUNCTION IF EXISTS reject_evidence_mutation();
DROP FUNCTION IF EXISTS reject_authorization_mutation();
