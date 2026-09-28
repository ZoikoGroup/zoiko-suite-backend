-- NCD-02: Suppression, Preference, and Channel Decision.
--
-- §5.3 "preference versus permission" separation: a recipient's preference
-- (what they want) is distinct from permission (what they consented to / legal
-- basis). A suppression is a hard stop — the channel is blocked regardless of
-- preference. Quiet hours are a temporal preference, not a suppression.
--
-- §5.4 suppression precedence: suppression > quiet hours > preference > permission.

CREATE TABLE IF NOT EXISTS suppressions (
    suppression_id     UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    principal_id       VARCHAR(255) NOT NULL,
    channel            VARCHAR(20) NOT NULL, -- EMAIL, SMS, IN_APP, WEBHOOK
    reason             TEXT NOT NULL,       -- unsubscribe, bounce, complaint, manual, legal_hold
    created_by         VARCHAR(255) NOT NULL, -- principal who created it (recipient or admin)
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ,         -- optional: temporary suppression

    CONSTRAINT suppressions_channel_known
        CHECK (channel IN ('EMAIL', 'SMS', 'IN_APP', 'WEBHOOK')),
    CONSTRAINT suppressions_reason_known
        CHECK (reason IN ('unsubscribe', 'bounce', 'complaint', 'manual', 'legal_hold')),
    CONSTRAINT suppressions_no_future_expiry
        CHECK (expires_at IS NULL OR expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS idx_suppressions_lookup
    ON suppressions (tenant_id, principal_id, channel)
    WHERE expires_at IS NULL OR expires_at > now();

ALTER TABLE suppressions ENABLE ROW LEVEL SECURITY;
ALTER TABLE suppressions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS suppressions_tenant_isolation ON suppressions;
CREATE POLICY suppressions_tenant_isolation ON suppressions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Preferences: what the recipient wants (not a hard block like suppression)
CREATE TABLE IF NOT EXISTS preferences (
    preference_id      UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    principal_id       VARCHAR(255) NOT NULL,
    channel            VARCHAR(20) NOT NULL, -- EMAIL, SMS, IN_APP, WEBHOOK
    enabled            BOOLEAN NOT NULL DEFAULT true,
    quiet_hours_start  TIME,                -- e.g. '22:00:00' (local time)
    quiet_hours_end    TIME,                -- e.g. '08:00:00' (local time)
    timezone           VARCHAR(50),         -- IANA timezone, e.g. 'Europe/London'
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT preferences_channel_known
        CHECK (channel IN ('EMAIL', 'SMS', 'IN_APP', 'WEBHOOK')),
    CONSTRAINT preferences_quiet_hours_together
        CHECK ((quiet_hours_start IS NULL) = (quiet_hours_end IS NULL)),
    CONSTRAINT preferences_timezone_when_quiet
        CHECK ((quiet_hours_start IS NULL) OR (timezone IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_preferences_lookup
    ON preferences (tenant_id, principal_id, channel);

ALTER TABLE preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE preferences FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS preferences_tenant_isolation ON preferences;
CREATE POLICY preferences_tenant_isolation ON preferences FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Channel decision log: audit trail for every channel decision
CREATE TABLE IF NOT EXISTS channel_decisions (
    decision_id        UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    principal_id       VARCHAR(255) NOT NULL,
    channel            VARCHAR(20) NOT NULL,
    decision           VARCHAR(20) NOT NULL, -- allowed, suppressed, quiet_hours, no_preference, no_permission
    suppression_id     UUID,                -- if suppressed, which suppression
    preference_id      UUID,                -- if preference checked
    permission_grant   VARCHAR(255),        -- legal basis / permission reference
    evaluated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT channel_decisions_channel_known
        CHECK (channel IN ('EMAIL', 'SMS', 'IN_APP', 'WEBHOOK')),
    CONSTRAINT channel_decisions_decision_known
        CHECK (decision IN ('allowed', 'suppressed', 'quiet_hours', 'no_preference', 'no_permission'))
);

CREATE INDEX IF NOT EXISTS idx_channel_decisions_lookup
    ON channel_decisions (tenant_id, principal_id, evaluated_at DESC);

ALTER TABLE channel_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_decisions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS channel_decisions_tenant_isolation ON channel_decisions;
CREATE POLICY channel_decisions_tenant_isolation ON channel_decisions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));