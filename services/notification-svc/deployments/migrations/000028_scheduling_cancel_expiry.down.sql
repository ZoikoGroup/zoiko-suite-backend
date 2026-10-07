-- Reverse of 000028. Communications already CANCELLED or EXPIRED, attempts with origin
-- 'scheduled' and unpublished communication.cancelled events would violate the narrower
-- constraints this restores, so convert or drain them first:
--
--   SELECT status, count(*) FROM notifications WHERE status IN ('CANCELLED','EXPIRED') GROUP BY 1;
--   SELECT count(*) FROM notification_delivery_attempts WHERE origin = 'scheduled';
--   SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND event_type = 'communication.cancelled';
DELETE FROM event_outbox WHERE published_at IS NOT NULL AND event_type = 'communication.cancelled';
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

ALTER TABLE notification_delivery_attempts DROP CONSTRAINT IF EXISTS notification_delivery_attempts_origin_check;
ALTER TABLE notification_delivery_attempts ADD CONSTRAINT notification_delivery_attempts_origin_check
    CHECK (origin IN ('request', 'retry', 'resend')) NOT VALID;
ALTER TABLE notification_delivery_attempts DROP COLUMN IF EXISTS job_id;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_expiry_has_reason;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_cancel_has_evidence;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_status_known;
ALTER TABLE notifications ADD CONSTRAINT notifications_status_known
    CHECK (status IN ('PENDING', 'SENT', 'FAILED', 'PENDING_UNKNOWN')) NOT VALID;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_timing_order;
ALTER TABLE notifications
    DROP COLUMN IF EXISTS cancel_reason,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS cancelled_by,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS not_before,
    DROP COLUMN IF EXISTS job_id;
