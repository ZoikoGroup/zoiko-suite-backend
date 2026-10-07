-- 000027_more_canonical_events.up.sql
-- ZS-SVC-Y-001 section 10.2: communication.prepared, communication.blocked, delivery.evidence.recorded,
-- endpoint.suppressed and notice.deadline.at_risk join the outbox allow-list, and a notice remembers
-- that its deadline warning was sent so it is raised once, not on every sweep.
ALTER TABLE regulated_notices ADD COLUMN IF NOT EXISTS at_risk_notified_at TIMESTAMPTZ;

ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent', 'notification.failed', 'notification.outcome_unknown',
        'template.created', 'template.version_approved', 'template.published', 'template.retired',
        'delivery.attempt.created', 'delivery.attempt.unknown',
        'notice.dispatched', 'notice.delivery_evidenced', 'notice.exception', 'notice.acknowledged',
        'notice.declined', 'notice.disputed', 'notice.expired', 'notice.corrected',
        'communication.prepared', 'communication.blocked', 'delivery.evidence.recorded',
        'endpoint.suppressed', 'notice.deadline.at_risk'
    ));
