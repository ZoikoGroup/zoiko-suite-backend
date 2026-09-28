-- Durable attempt records and expanded delivery status per ZS-SVC-Y-001 §3.3, §3.4.
--
-- The register previously had only a counter (delivery_attempts INT) and three
-- status values (PENDING, SENT, FAILED). §3.3 requires five distinct claims:
-- PROVIDER_ACCEPTED, DELIVERED, READ, SERVED — never one generic "sent=true".
-- §3.4 requires a durable attempt_id per provider submission, with resend
-- reason and evidence chain preserved.

-- ── delivery_attempts table ───────────────────────────────────────────────────
--
-- One row per provider submission. The notification table keeps a counter for
-- compatibility and quick reads, but the authoritative chain is here.

CREATE TABLE IF NOT EXISTS delivery_attempts (
    attempt_id           UUID PRIMARY KEY,
    notification_id      UUID NOT NULL REFERENCES notifications(notification_id) ON DELETE CASCADE,
    attempt_number       INT NOT NULL,
    channel              VARCHAR(20) NOT NULL,
    provider             VARCHAR(50) NOT NULL,
    status               VARCHAR(30) NOT NULL, -- UNKNOWN, PROVIDER_ACCEPTED, DELIVERED, READ, SERVED, FAILED
    provider_response    TEXT,
    failure_reason       TEXT,
    retryable            BOOLEAN NOT NULL DEFAULT true,
    resend_reason        VARCHAR(30) NOT NULL, -- first_try, retry, reconcile, manual
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    concluded_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_delivery_attempts_notification
    ON delivery_attempts (notification_id, attempt_number);

-- ── Expand notifications.status constraint ────────────────────────────────────
--
-- The new values implement ZS-SVC-Y-001 §3.3: SENT is never a single status.
-- NOT VALID for the reason 000002 sets out: it enforces on new writes and
-- skips the scan that would reject existing rows.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_status_known'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            DROP CONSTRAINT IF EXISTS notifications_status_known;
    END IF;

    ALTER TABLE notifications
        ADD CONSTRAINT notifications_status_known
        CHECK (status IN (
            'PENDING',
            'UNKNOWN',
            'PROVIDER_ACCEPTED',
            'DELIVERED',
            'READ',
            'SERVED',
            'FAILED'
        )) NOT VALID;
END $$;

-- ── Conclusions must have timestamps for all non-PENDING states ──────────────
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_concluded_has_timestamp'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            DROP CONSTRAINT IF EXISTS notifications_concluded_has_timestamp;
    END IF;

    ALTER TABLE notifications
        ADD CONSTRAINT notifications_concluded_has_timestamp
        CHECK (status = 'PENDING' OR sent_at IS NOT NULL) NOT VALID;
END $$;

-- ── Add purpose_context and idempotency_key columns if missing ──────────────
-- (Added by 000003 but ensuring they exist for the idempotency index below)
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(500);
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS purpose_context VARCHAR(255);

-- ── Replace the old (tenant_id, correlation_id) index with purpose-scoped one ─
--
-- The old index caused collisions when two different communications shared a
-- correlation_id but had different purposes (§3.4). The new index includes
-- purpose_context so they remain distinct.

DROP INDEX IF EXISTS idx_notifications_tenant_correlation;

CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_tenant_idempotency
    ON notifications (tenant_id, idempotency_key);