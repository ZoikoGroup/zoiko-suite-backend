-- Reverse of 000014. Drain delivery.attempt.* events from event_outbox first,
-- or restoring the narrower CHECK fails on the rows still there:
--
--   SELECT count(*) FROM event_outbox
--   WHERE published_at IS NULL AND event_type LIKE 'delivery.attempt.%';
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent',
        'notification.failed',
        'notification.outcome_unknown',
        'template.created',
        'template.version_approved',
        'template.published',
        'template.retired'
    ));
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_resend_count_non_negative;
ALTER TABLE notifications DROP COLUMN IF EXISTS last_resent_by_principal_id;
ALTER TABLE notifications DROP COLUMN IF EXISTS last_resent_at;
ALTER TABLE notifications DROP COLUMN IF EXISTS last_resend_reason;
ALTER TABLE notifications DROP COLUMN IF EXISTS resend_count;
