-- 000008_suppression_and_action_tokens.up.sql
-- Suppression and Link-Scanner Safe Action Tokens for ZoikoSuite Email Communications System (ZS-COMMS-EMAIL-001 v2.0 §4, §6)

-- 1. email_suppressions: tracks hard bounces, spam complaints, user unsubscribes, and administrative suppressions
CREATE TABLE IF NOT EXISTS email_suppressions (
    suppression_id      UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    recipient_email     TEXT NOT NULL,
    reason              VARCHAR(32) NOT NULL,
    source_stream       VARCHAR(32) NOT NULL DEFAULT 'ALL',
    provider_name       VARCHAR(64),
    raw_metadata        JSONB,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

ALTER TABLE email_suppressions ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_suppressions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON email_suppressions;
CREATE POLICY tenant_isolation_policy ON email_suppressions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE UNIQUE INDEX IF NOT EXISTS idx_email_suppressions_tenant_email_stream
    ON email_suppressions (tenant_id, recipient_email, source_stream);

CREATE INDEX IF NOT EXISTS idx_email_suppressions_tenant_lookup
    ON email_suppressions (tenant_id, recipient_email);

CREATE INDEX IF NOT EXISTS idx_email_suppressions_reason
    ON email_suppressions (tenant_id, reason);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'email_suppressions_reason_known') THEN
        ALTER TABLE email_suppressions
            ADD CONSTRAINT email_suppressions_reason_known
            CHECK (reason IN ('HARD_BOUNCE', 'COMPLAINT', 'UNSUBSCRIBE', 'ADMIN_SUPPRESSED')) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'email_suppressions_stream_known') THEN
        ALTER TABLE email_suppressions
            ADD CONSTRAINT email_suppressions_stream_known
            CHECK (source_stream IN ('ALL', 'MARKETING', 'OPERATIONAL', 'TRANSACTIONAL', 'CRITICAL')) NOT VALID;
    END IF;
END $$;


-- 2. action_tokens: signed, purpose-bound, link-scanner safe single-use tokens
CREATE TABLE IF NOT EXISTS action_tokens (
    token_id                UUID PRIMARY KEY,
    token_hash              VARCHAR(64) NOT NULL,
    message_intent_id       UUID NOT NULL,
    tenant_id               VARCHAR(255) NOT NULL,
    recipient_principal_id  VARCHAR(255) NOT NULL,
    purpose                 VARCHAR(64) NOT NULL,
    target_action_url       TEXT NOT NULL,
    target_method           VARCHAR(10) NOT NULL DEFAULT 'POST',
    payload                 JSONB,
    status                  VARCHAR(20) NOT NULL DEFAULT 'ACTIVE',
    expires_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    consumed_at             TIMESTAMP WITH TIME ZONE,
    consumed_by_ip          TEXT,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint 
        WHERE conname = 'action_tokens_message_intent_id_fkey'
    ) THEN
        ALTER TABLE action_tokens
            ADD CONSTRAINT action_tokens_message_intent_id_fkey
            FOREIGN KEY (message_intent_id) REFERENCES message_intents(message_intent_id) ON DELETE CASCADE;
    END IF;
END $$;

ALTER TABLE action_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE action_tokens FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON action_tokens;
CREATE POLICY tenant_isolation_policy ON action_tokens FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE UNIQUE INDEX IF NOT EXISTS idx_action_tokens_hash
    ON action_tokens (token_hash);

CREATE INDEX IF NOT EXISTS idx_action_tokens_intent
    ON action_tokens (tenant_id, message_intent_id);

CREATE INDEX IF NOT EXISTS idx_action_tokens_recipient
    ON action_tokens (tenant_id, recipient_principal_id);

CREATE INDEX IF NOT EXISTS idx_action_tokens_status_expiry
    ON action_tokens (tenant_id, status, expires_at);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'action_tokens_status_known') THEN
        ALTER TABLE action_tokens
            ADD CONSTRAINT action_tokens_status_known
            CHECK (status IN ('ACTIVE', 'CONSUMED', 'EXPIRED', 'REVOKED')) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'action_tokens_method_known') THEN
        ALTER TABLE action_tokens
            ADD CONSTRAINT action_tokens_method_known
            CHECK (target_method IN ('POST', 'PUT', 'PATCH', 'DELETE')) NOT VALID;
    END IF;
END $$;
