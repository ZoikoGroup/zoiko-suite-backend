-- ZS-SVC-Y-001 §10.2 canonical events. Every one is enqueued in the
-- transaction that records the fact it describes (migration 000010).
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
        'communication.prepared',
        'communication.blocked',
        'delivery.evidence.recorded',
        'endpoint.suppressed',
        'notice.acknowledged',
        'notice.deadline.at_risk',
        'communication.correction.issued',
        'communication.record.declared'
    ));
