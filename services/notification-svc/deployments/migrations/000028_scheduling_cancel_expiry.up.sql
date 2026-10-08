-- 000028_scheduling_cancel_expiry.up.sql
-- ZS-SVC-Y-001 NCD-03 sections 6.1 and 6.6: the delivery job's identity and server-authoritative
-- timing, and cancellation. NP-52 (a job that expires before it is submitted).
--
-- job_id        the channel-plan execution container. Today a communication has exactly one, so
--               it is a stable identity on the communication that every attempt carries; a
--               separate job table can follow when governed fallback gives a job its own life.
-- not_before    do not submit before this instant (a scheduled send).
-- expires_at    never submit after this instant: a stale reminder or notice is worse than none.
-- cancelled_*   who withdrew a queued communication, when and why (evidence, never deleted).
--
-- Two new terminal statuses, CANCELLED and EXPIRED. Both conclude a communication that was
-- never submitted, so both carry a reason, and neither can be retried or resent.

ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS job_id        UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN IF NOT EXISTS not_before    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS expires_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancelled_by  VARCHAR(255),
    ADD COLUMN IF NOT EXISTS cancelled_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancel_reason TEXT;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_timing_order;
ALTER TABLE notifications ADD CONSTRAINT notifications_timing_order
    CHECK (not_before IS NULL OR expires_at IS NULL OR expires_at > not_before);

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_status_known;
ALTER TABLE notifications ADD CONSTRAINT notifications_status_known
    CHECK (status IN ('PENDING', 'SENT', 'FAILED', 'PENDING_UNKNOWN', 'CANCELLED', 'EXPIRED')) NOT VALID;

-- A communication withdrawn or expired before submission says why, and a cancellation says who.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_cancel_has_evidence;
ALTER TABLE notifications ADD CONSTRAINT notifications_cancel_has_evidence
    CHECK (status <> 'CANCELLED' OR (cancelled_by IS NOT NULL AND cancelled_at IS NOT NULL
        AND cancel_reason IS NOT NULL AND length(btrim(cancel_reason)) > 0)) NOT VALID;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_expiry_has_reason;
ALTER TABLE notifications ADD CONSTRAINT notifications_expiry_has_reason
    CHECK (status <> 'EXPIRED' OR (expires_at IS NOT NULL AND failure_reason IS NOT NULL AND failure_reason <> '')) NOT VALID;

-- The job a communication ran under, on every attempt (the canonical delivery.attempt.created
-- event carries it). 'scheduled' marks the first attempt of a send that waited for not_before.
ALTER TABLE notification_delivery_attempts ADD COLUMN IF NOT EXISTS job_id UUID;
ALTER TABLE notification_delivery_attempts DROP CONSTRAINT IF EXISTS notification_delivery_attempts_origin_check;
ALTER TABLE notification_delivery_attempts ADD CONSTRAINT notification_delivery_attempts_origin_check
    CHECK (origin IN ('request', 'retry', 'resend', 'scheduled'));

ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent', 'notification.failed', 'notification.outcome_unknown',
        'template.created', 'template.version_approved', 'template.published', 'template.retired',
        'delivery.attempt.created', 'delivery.attempt.unknown',
        'notice.dispatched', 'notice.delivery_evidenced', 'notice.exception', 'notice.acknowledged',
        'notice.declined', 'notice.disputed', 'notice.expired', 'notice.corrected',
        'communication.prepared', 'communication.blocked', 'delivery.evidence.recorded',
        'endpoint.suppressed', 'notice.deadline.at_risk',
        'communication.cancelled'
    ));
