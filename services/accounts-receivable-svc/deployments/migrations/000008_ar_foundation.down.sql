DROP TABLE IF EXISTS ar_accounting_posting_requests;
DROP TRIGGER IF EXISTS ar_audit_events_append_only ON ar_audit_events;
DROP TABLE IF EXISTS ar_audit_events;
DROP FUNCTION IF EXISTS ar_forbid_mutation();
DROP TABLE IF EXISTS ar_command_idempotency;
