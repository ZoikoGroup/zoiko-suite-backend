-- Migration: 000006_audit_events_immutability_and_hash_scheme.down.sql

DROP TRIGGER IF EXISTS trg_audit_events_reject_truncate ON audit_events;
DROP TRIGGER IF EXISTS trg_audit_events_reject_delete ON audit_events;
DROP TRIGGER IF EXISTS trg_audit_events_reject_update ON audit_events;
DROP FUNCTION IF EXISTS reject_audit_event_mutation();

ALTER TABLE audit_events DROP COLUMN IF EXISTS payload_hash_scheme;
