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
