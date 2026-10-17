-- Dropping these discards delivery evidence and the inbox receipts that keep the
-- event fan-out exactly-once; do it only to undo a deployment that never delivered
-- to a real destination.
DROP TABLE IF EXISTS webhook_delivery_attempts;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_subscription_log;
DROP TABLE IF EXISTS webhook_subscriptions;
DROP FUNCTION IF EXISTS webhook_attempt_guard();
DROP FUNCTION IF EXISTS webhook_delivery_guard();
DROP FUNCTION IF EXISTS webhook_sub_guard();
DROP FUNCTION IF EXISTS webhook_log_immutable();
DROP TABLE IF EXISTS eventing_inbox;
DROP TABLE IF EXISTS eventing_repair_log;
DROP FUNCTION IF EXISTS eventing_repair_log_immutable();
-- Restore the event list as 000030 left it.
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent', 'notification.failed', 'notification.outcome_unknown', 'template.created',
        'template.version_approved', 'template.published', 'template.retired', 'delivery.attempt.created',
        'delivery.attempt.unknown', 'notice.dispatched', 'notice.delivery_evidenced', 'notice.exception',
        'notice.acknowledged', 'notice.declined', 'notice.disputed', 'notice.expired', 'notice.corrected',
        'communication.prepared', 'communication.blocked', 'delivery.evidence.recorded', 'endpoint.suppressed',
        'notice.deadline.at_risk', 'communication.cancelled', 'communication.correction.issued',
        'communication.record.declared'
    ));
