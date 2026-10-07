-- Reverse of 000025. Drain notice.* events from event_outbox first, or restoring the
-- narrower CHECK fails on the rows still there:
--
--   SELECT count(*) FROM event_outbox WHERE event_type LIKE 'notice.%';
DELETE FROM event_outbox WHERE event_type LIKE 'notice.%' AND published_at IS NOT NULL;
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent',
        'notification.failed',
        'notification.outcome_unknown',
        'template.created',
        'template.version_approved',
        'template.published',
        'template.retired',
        'delivery.attempt.created',
        'delivery.attempt.unknown'
    ));

DROP TABLE IF EXISTS notice_acknowledgements;
DROP TABLE IF EXISTS regulated_notice_events;
DROP TABLE IF EXISTS regulated_notices;
DROP FUNCTION IF EXISTS guard_notice_append_only();
DROP FUNCTION IF EXISTS guard_regulated_notice();
