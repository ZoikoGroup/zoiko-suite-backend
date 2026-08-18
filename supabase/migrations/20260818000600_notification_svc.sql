-- 20260818000600_notification_svc.sql
-- notification-svc → schema `notification`
--
-- Squashed end state of 000001_initial_schema and
-- 000002_force_rls_and_constraints. One table: notifications.
--
-- ── The NOT VALID constraints become VALID here ──────────────────────────────
-- 000002 added every CHECK as NOT VALID for a specific reason: the dev database
-- holds rows the old delivery adapter wrote when it treated an unrecognised
-- channel as a delivery failure — e.g. (channel 'PIGEON', status 'FAILED',
-- failure_reason 'unsupported channel: PIGEON'). Those rows are the audit trail
-- of what the service actually did, wrong as it was, and a migration must not
-- quietly rewrite them to make a constraint pass.
--
-- That is an argument about an existing table with a backlog. This database has
-- none, so the constraints are declared normally and the open "run VALIDATE
-- CONSTRAINT once the backlog is dealt with" item does not travel here.
--
-- ── Still true on Supabase ───────────────────────────────────────────────────
-- Delivery goes through a documented STUB adapter. No real provider is wired,
-- so a row reading SENT means "recorded and accepted by the stub", not
-- "delivered". Nothing in this migration changes that.

CREATE SCHEMA IF NOT EXISTS notification;

COMMENT ON SCHEMA notification IS
    'notification-svc. Register of notifications and their delivery outcome. Delivery is via a stub adapter — SENT means accepted by the stub, not delivered.';

GRANT USAGE ON SCHEMA notification TO zoiko_backend, authenticated;

-- ── notifications ────────────────────────────────────────────────────────────

CREATE TABLE notification.notifications (
    notification_id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,

    recipient_principal_id  VARCHAR(255) NOT NULL,

    -- EMAIL | SMS | IN_APP | WEBHOOK
    channel                 VARCHAR(20)  NOT NULL,

    subject                 VARCHAR(255) NOT NULL,
    body                    TEXT         NOT NULL,

    -- PENDING | SENT | FAILED
    status                  VARCHAR(20)  NOT NULL,

    source_event_type       VARCHAR(100),
    source_reference        VARCHAR(255),
    correlation_id          VARCHAR(255) NOT NULL,
    failure_reason          TEXT,

    created_by_principal_id VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    sent_at                 TIMESTAMPTZ,

    -- The vocabulary the domain defines, enforced in Go and — before these —
    -- nowhere else, so any other writer could leave a value no consumer knows
    -- how to read.
    CONSTRAINT notifications_channel_known
        CHECK (channel IN ('EMAIL', 'SMS', 'IN_APP', 'WEBHOOK')),
    CONSTRAINT notifications_status_known
        CHECK (status IN ('PENDING', 'SENT', 'FAILED')),

    -- A concluded delivery must say when it concluded, and a FAILED one must
    -- say why. A FAILED row with no reason is a record that something did not
    -- go out and no account of what happened — which is the only thing that
    -- row is for.
    CONSTRAINT notifications_concluded_has_timestamp
        CHECK (status = 'PENDING' OR sent_at IS NOT NULL),
    CONSTRAINT notifications_failed_has_reason
        CHECK (status <> 'FAILED' OR (failure_reason IS NOT NULL AND failure_reason <> ''))
);

-- Idempotency: a retried send resolves to the original notification, never a
-- duplicate send.
CREATE UNIQUE INDEX idx_notifications_tenant_correlation
    ON notification.notifications (tenant_id, correlation_id);

CREATE INDEX idx_notifications_tenant_entity
    ON notification.notifications (tenant_id, legal_entity_id);
CREATE INDEX idx_notifications_tenant_recipient
    ON notification.notifications (tenant_id, recipient_principal_id);
CREATE INDEX idx_notifications_tenant_status
    ON notification.notifications (tenant_id, status);

-- The register is read newest-first and paged; created_at alone is not a total
-- order, so the index carries the primary key as a tiebreaker for the same
-- reason the ORDER BY does.
CREATE INDEX idx_notifications_tenant_created
    ON notification.notifications (tenant_id, created_at DESC, notification_id DESC);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE notification.notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification.notifications FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON notification.notifications
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

-- A recipient's own notifications are readable by that recipient; everything
-- else in the tenant is not. Tighter than the other services' tenant-wide read
-- because the body of a notification is addressed to one person.
CREATE POLICY recipient_read ON notification.notifications
    FOR SELECT
    TO authenticated
    USING (
        tenant_id = app.current_tenant_id()
        AND recipient_principal_id = app.current_principal_id()
    );

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON notification.notifications TO authenticated;

-- No DELETE: the register is the account of what was sent, including what
-- failed to send.
GRANT SELECT, INSERT, UPDATE ON notification.notifications TO zoiko_backend;
