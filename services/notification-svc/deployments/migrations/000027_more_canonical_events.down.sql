-- Reverse of 000027. Events of the removed types that were already published are dropped; an
-- unpublished one would make restoring the narrower CHECK fail, so drain the relay first:
--
--   SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND event_type IN (
--     'communication.prepared', 'communication.blocked', 'delivery.evidence.recorded',
--     'endpoint.suppressed', 'notice.deadline.at_risk');
DELETE FROM event_outbox WHERE published_at IS NOT NULL AND event_type IN
    ('communication.prepared', 'communication.blocked', 'delivery.evidence.recorded', 'endpoint.suppressed', 'notice.deadline.at_risk');
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent', 'notification.failed', 'notification.outcome_unknown',
        'template.created', 'template.version_approved', 'template.published', 'template.retired',
        'delivery.attempt.created', 'delivery.attempt.unknown',
        'notice.dispatched', 'notice.delivery_evidenced', 'notice.exception', 'notice.acknowledged',
        'notice.declined', 'notice.disputed', 'notice.expired', 'notice.corrected'
    ));
ALTER TABLE regulated_notices DROP COLUMN IF EXISTS at_risk_notified_at;
