-- Durable per-attempt records for the direct-send path (ZS-SVC-Y-001 §3.4, §6.2).
--
-- §3.4: "Every provider submission has a separate durable attempt_id." Until
-- now the /v1/notifications path kept only a counter (notifications.
-- delivery_attempts) and the LAST attempt's failure_reason and
-- provider_response, each overwritten by the next. A notification delivered on
-- its fourth try therefore had no record of what the first three were told,
-- and a PENDING_UNKNOWN row had no record of which submission is ambiguous —
-- the one thing §6.2 says must be reconciled before anything is re-sent.
--
-- WHY A NEW TABLE, NOT delivery_attempts. Migration 000007 already created
-- delivery_attempts, and it belongs to the delivery ledger: every row hangs off
-- a message_intents/message_renders pair (both NOT NULL) and is written only by
-- the ingestion orchestrator. The direct-send path has no intent or render row
-- to point at. Relaxing those foreign keys would weaken the ledger's own
-- lineage guarantee to accommodate a different path, so the two paths keep
-- separate attempt tables with one shared vocabulary for the outcome.
--
-- Written by the same transaction as the transition it records —
-- CompleteDelivery, ScheduleRetry and MarkOutcomeUnknown each insert exactly
-- one row — so an attempt that changed a notification's state always has its
-- record, and a record never exists for an attempt whose outcome was not kept.

CREATE TABLE IF NOT EXISTS notification_delivery_attempts (
    attempt_id        UUID PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL,
    notification_id   UUID NOT NULL REFERENCES notifications (notification_id) ON DELETE CASCADE,

    -- 1-based, equal to notifications.delivery_attempts after this attempt.
    attempt_number    INTEGER NOT NULL CHECK (attempt_number >= 1),

    -- Where the attempt was made. 'resend' is an explicit, reasoned resend
    -- (§3.4: "User-initiated resend creates an explicit resend reason and
    -- preserves the original attempt/evidence chain").
    origin            VARCHAR(20) NOT NULL
        CHECK (origin IN ('request', 'retry', 'resend')),

    channel           VARCHAR(20) NOT NULL,
    provider_name     VARCHAR(100),

    -- What this one attempt achieved. Not the notification's status:
    -- RETRYING means this attempt failed and another is scheduled.
    outcome           VARCHAR(20) NOT NULL
        CHECK (outcome IN ('ACCEPTED', 'FAILED', 'RETRYING', 'UNKNOWN')),

    provider_response TEXT,
    failure_reason    TEXT,
    retryable         BOOLEAN NOT NULL DEFAULT false,

    -- Required for a resend, and only there: an explicit reason is the
    -- evidence §3.4 asks for, and inventing one for an automatic retry would
    -- make the column meaningless.
    resend_reason     TEXT,

    -- Who caused a resend. Automatic attempts carry the principal that
    -- originated the send.
    actor_principal_id VARCHAR(255),

    attempted_at      TIMESTAMPTZ NOT NULL,
    recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT nda_attempt_number_unique UNIQUE (notification_id, attempt_number),
    CONSTRAINT nda_resend_has_reason CHECK (
        (origin = 'resend' AND resend_reason IS NOT NULL AND resend_reason <> '')
        OR (origin <> 'resend' AND resend_reason IS NULL)
    ),
    CONSTRAINT nda_failure_has_reason CHECK (
        outcome = 'ACCEPTED' OR (failure_reason IS NOT NULL AND failure_reason <> '')
    )
);

CREATE INDEX IF NOT EXISTS idx_nda_notification
    ON notification_delivery_attempts (tenant_id, notification_id, attempt_number);

ALTER TABLE notification_delivery_attempts ENABLE ROW LEVEL SECURITY;
-- FORCE, for the reason 000002 gives: the service connects as the table owner,
-- and an owner is exempt from row-level security unless FORCE is declared.
ALTER TABLE notification_delivery_attempts FORCE ROW LEVEL SECURITY;

-- NULLIF guards the empty-GUC trap: a pooled connection that has served one
-- request keeps app.tenant_id as '' rather than NULL after the transaction.
DROP POLICY IF EXISTS nda_tenant_isolation ON notification_delivery_attempts;
CREATE POLICY nda_tenant_isolation ON notification_delivery_attempts FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- The retry worker's discovery hatch (app.platform_scope, 000004) is
-- SELECT-only here too: the reconcile-before-retry check reads the last
-- attempt of a claimed row, and every write stays tenant-scoped.
DROP POLICY IF EXISTS nda_platform_scope_read ON notification_delivery_attempts;
CREATE POLICY nda_platform_scope_read ON notification_delivery_attempts FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.platform_scope', true), ''), 'false') = 'true');
