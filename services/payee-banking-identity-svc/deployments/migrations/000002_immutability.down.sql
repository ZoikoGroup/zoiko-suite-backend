DROP TRIGGER IF EXISTS trg_reject_payee_destination_events_mutation ON payee_destination_events;
DROP FUNCTION IF EXISTS reject_payee_event_mutation();
DROP TRIGGER IF EXISTS trg_reject_payee_destination_mutation ON payee_destinations;
DROP FUNCTION IF EXISTS reject_payee_destination_mutation();
