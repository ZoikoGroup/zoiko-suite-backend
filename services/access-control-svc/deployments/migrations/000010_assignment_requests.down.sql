-- Migration 000010 down.
DELETE FROM event_outbox WHERE event_type LIKE 'iam.%' AND published_at IS NULL;
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN ('role.created', 'role.updated', 'permission.bundle.updated')) NOT VALID;
DROP TABLE IF EXISTS assignment_requests;
