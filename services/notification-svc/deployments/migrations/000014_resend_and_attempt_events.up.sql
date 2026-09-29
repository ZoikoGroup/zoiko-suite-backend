-- Reasoned resend, and the spec's attempt-level events (ZS-SVC-Y-001 §3.4, §10.2).
--
-- §3.4: "User-initiated 'resend' creates an explicit resend reason and
-- preserves the original attempt/evidence chain." A resend reopens a SENT or
-- FAILED notification for ONE more governed attempt on the SAME communication —
-- not a new, unrelated notification — so the attempt chain in
-- notification_delivery_attempts (000011) stays one chain. The reason lives on
-- the attempt row (nda_resend_has_reason refuses a resend without one); the
-- columns below are the notification's own summary of it.
--
-- A PENDING or PENDING_UNKNOWN notification cannot be resent: the first is
-- still being attempted, and the second must be resolved against the
-- provider's records first — resending it is the blind second material send
-- §6.2 forbids.

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS resend_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS last_resend_reason TEXT;
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS last_resent_at TIMESTAMPTZ;
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS last_resent_by_principal_id VARCHAR(255);

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_resend_count_non_negative;
ALTER TABLE notifications ADD CONSTRAINT notifications_resend_count_non_negative
    CHECK (resend_count >= 0) NOT VALID;

-- §10.2 canonical attempt events. delivery.attempt.created is enqueued for every
-- durable attempt row, in the transaction that writes it, so attempt-level
-- evidence (attempt_id, channel, provider, origin, resend reason) reaches
-- consumers without widening notification.* payloads. delivery.attempt.unknown
-- additionally carries the resolution deadline for an ambiguous attempt.
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
