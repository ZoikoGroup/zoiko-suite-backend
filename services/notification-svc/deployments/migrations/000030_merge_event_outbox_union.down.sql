-- 000030_merge_event_outbox_union.down.sql
-- Restores the list 000028 leaves when the lineages are applied in sorted order.

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
        'delivery.attempt.unknown',
        'notice.dispatched',
        'notice.delivery_evidenced',
        'notice.exception',
        'notice.acknowledged',
        'notice.declined',
        'notice.disputed',
        'notice.expired',
        'notice.corrected',
        'communication.prepared',
        'communication.blocked',
        'delivery.evidence.recorded',
        'endpoint.suppressed',
        'notice.deadline.at_risk',
        'communication.cancelled'
    ));
