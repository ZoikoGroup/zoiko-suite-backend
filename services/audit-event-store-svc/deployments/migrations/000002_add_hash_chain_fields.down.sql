DROP INDEX IF EXISTS audit_events_correlation_id_idx;
DROP INDEX IF EXISTS audit_events_sequence_number_desc_idx;
DROP INDEX IF EXISTS audit_events_sequence_number_idx;

ALTER TABLE audit_events
    DROP COLUMN IF EXISTS previous_event_hash,
    DROP COLUMN IF EXISTS payload_hash,
    DROP COLUMN IF EXISTS sequence_number,
    DROP COLUMN IF EXISTS causation_id,
    DROP COLUMN IF EXISTS correlation_id;
