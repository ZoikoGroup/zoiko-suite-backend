-- NCD-04: Bounce, Complaint, and Channel Reputation.
--
-- §7 Bounce handling, complaint handling, channel reputation and suppression state.
-- These are missing entirely.

CREATE TABLE IF NOT EXISTS bounce_events (
    bounce_id          UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    notification_id    UUID NOT NULL REFERENCES notifications(notification_id) ON DELETE CASCADE,
    provider           VARCHAR(50) NOT NULL,           -- smtp, ses, sendgrid, etc.
    bounce_type        VARCHAR(30) NOT NULL,           -- hard, soft, transient
    bounce_subtype     VARCHAR(50),                    -- mailbox_full, unknown_user, etc.
    diagnostic_code    TEXT,                           -- SMTP diagnostic code
    recipient_address  TEXT NOT NULL,                  -- the address that bounced
    received_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at       TIMESTAMPTZ,                    -- when we acted on it
    action_taken       VARCHAR(30),                    -- suppression_created, retry_scheduled, ignored
    suppression_id     UUID REFERENCES suppressions(suppression_id) ON DELETE SET NULL,

    CONSTRAINT bounce_events_bounce_type_known
        CHECK (bounce_type IN ('hard', 'soft', 'transient')),
    CONSTRAINT bounce_events_action_known
        CHECK (action_taken IS NULL OR action_taken IN ('suppression_created', 'retry_scheduled', 'ignored'))
);

CREATE INDEX IF NOT EXISTS idx_bounce_events_notification
    ON bounce_events (notification_id);

CREATE INDEX IF NOT EXISTS idx_bounce_events_recipient
    ON bounce_events (tenant_id, recipient_address, received_at DESC);

ALTER TABLE bounce_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE bounce_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS bounce_events_tenant_isolation ON bounce_events;
CREATE POLICY bounce_events_tenant_isolation ON bounce_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Complaint events (feedback loop / FBL)
CREATE TABLE IF NOT EXISTS complaint_events (
    complaint_id       UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    notification_id    UUID REFERENCES notifications(notification_id) ON DELETE SET NULL,
    provider           VARCHAR(50) NOT NULL,
    complaint_type     VARCHAR(50) NOT NULL,           -- abuse, spam, unsubscribe_request
    recipient_address  TEXT NOT NULL,
    received_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at       TIMESTAMPTZ,
    action_taken       VARCHAR(30),                    -- suppression_created, ignored
    suppression_id     UUID REFERENCES suppressions(suppression_id) ON DELETE SET NULL,

    CONSTRAINT complaint_events_action_known
        CHECK (action_taken IS NULL OR action_taken IN ('suppression_created', 'ignored'))
);

CREATE INDEX IF NOT EXISTS idx_complaint_events_notification
    ON complaint_events (notification_id);

CREATE INDEX IF NOT EXISTS idx_complaint_events_recipient
    ON complaint_events (tenant_id, recipient_address, received_at DESC);

ALTER TABLE complaint_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE complaint_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS complaint_events_tenant_isolation ON complaint_events;
CREATE POLICY complaint_events_tenant_isolation ON complaint_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Channel reputation: tracks delivery health per channel/provider
CREATE TABLE IF NOT EXISTS channel_reputation (
    reputation_id      UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    channel            VARCHAR(20) NOT NULL,
    provider           VARCHAR(50) NOT NULL,
    window_start       TIMESTAMPTZ NOT NULL,           -- e.g., daily/hourly window
    window_end         TIMESTAMPTZ NOT NULL,
    sent_count         BIGINT NOT NULL DEFAULT 0,
    accepted_count     BIGINT NOT NULL DEFAULT 0,
    bounced_count      BIGINT NOT NULL DEFAULT 0,
    complained_count   BIGINT NOT NULL DEFAULT 0,
    delivered_count    BIGINT NOT NULL DEFAULT 0,      -- confirmed delivery (DSN)
    read_count         BIGINT NOT NULL DEFAULT 0,      -- IN_APP read or read receipt
    reputation_score   NUMERIC(4,3),                   -- 0.000 to 1.000
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT channel_reputation_channel_known
        CHECK (channel IN ('EMAIL', 'SMS', 'IN_APP', 'WEBHOOK')),
    CONSTRAINT channel_reputation_window_order
        CHECK (window_start < window_end)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_reputation_unique
    ON channel_reputation (tenant_id, channel, provider, window_start);

CREATE INDEX IF NOT EXISTS idx_channel_reputation_tenant_window
    ON channel_reputation (tenant_id, window_start DESC);

ALTER TABLE channel_reputation ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_reputation FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS channel_reputation_tenant_isolation ON channel_reputation;
CREATE POLICY channel_reputation_tenant_isolation ON channel_reputation FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));