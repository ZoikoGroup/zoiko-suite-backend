-- 000005_delivery_ledger.up.sql
-- Canonical Delivery Ledger for ZoikoSuite Email Communications System (ZS-COMMS-EMAIL-001 v2.0 §3)

-- 1. message_intents: records the immutable communication intent for an event and recipient
CREATE TABLE IF NOT EXISTS message_intents (
    message_intent_id       UUID PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    recipient_principal_id   VARCHAR(255) NOT NULL,
    recipient_email         TEXT NOT NULL,
    channel                 VARCHAR(20) NOT NULL DEFAULT 'EMAIL',
    communication_class     VARCHAR(10) NOT NULL,
    template_key            VARCHAR(100) NOT NULL,
    event_id                VARCHAR(255),
    source_event_type       VARCHAR(100) NOT NULL,
    deduplication_key       VARCHAR(255) NOT NULL,
    correlation_id          VARCHAR(255) NOT NULL,
    causation_id            VARCHAR(255),
    status                  VARCHAR(30) NOT NULL DEFAULT 'PENDING',
    failure_reason          TEXT,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    updated_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

ALTER TABLE message_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE message_intents FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON message_intents FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE UNIQUE INDEX IF NOT EXISTS idx_message_intents_tenant_dedup
    ON message_intents (tenant_id, deduplication_key);

CREATE INDEX IF NOT EXISTS idx_message_intents_tenant_created
    ON message_intents (tenant_id, created_at DESC, message_intent_id DESC);

CREATE INDEX IF NOT EXISTS idx_message_intents_event_id
    ON message_intents (tenant_id, event_id);

CREATE INDEX IF NOT EXISTS idx_message_intents_status
    ON message_intents (tenant_id, status);

ALTER TABLE message_intents
    ADD CONSTRAINT message_intents_channel_known
    CHECK (channel IN ('EMAIL', 'IN_APP')) NOT VALID;

ALTER TABLE message_intents
    ADD CONSTRAINT message_intents_class_known
    CHECK (communication_class IN ('S0', 'T0', 'A1', 'L1', 'M1')) NOT VALID;

ALTER TABLE message_intents
    ADD CONSTRAINT message_intents_status_known
    CHECK (status IN ('PENDING', 'RENDERED', 'DISPATCHED', 'DELIVERED', 'FAILED', 'KILLED')) NOT VALID;


-- 2. message_renders: exact resolved template, locale, variables and SHA-256 content hash
CREATE TABLE IF NOT EXISTS message_renders (
    render_id               UUID PRIMARY KEY,
    message_intent_id       UUID NOT NULL REFERENCES message_intents(message_intent_id) ON DELETE CASCADE,
    tenant_id               VARCHAR(255) NOT NULL,
    template_key            VARCHAR(100) NOT NULL,
    template_version        VARCHAR(32) NOT NULL,
    locale                  VARCHAR(16) NOT NULL DEFAULT 'en-US',
    content_hash            VARCHAR(64) NOT NULL,
    subject                 VARCHAR(255) NOT NULL,
    body_html               TEXT NOT NULL,
    body_text               TEXT NOT NULL,
    rendered_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

ALTER TABLE message_renders ENABLE ROW LEVEL SECURITY;
ALTER TABLE message_renders FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON message_renders FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_message_renders_intent
    ON message_renders (tenant_id, message_intent_id);


-- 3. delivery_attempts: provider submissions and retry attempts with sender stream isolation
CREATE TABLE IF NOT EXISTS delivery_attempts (
    provider_attempt_id     UUID PRIMARY KEY,
    message_intent_id       UUID NOT NULL REFERENCES message_intents(message_intent_id) ON DELETE CASCADE,
    render_id               UUID NOT NULL REFERENCES message_renders(render_id) ON DELETE CASCADE,
    tenant_id               VARCHAR(255) NOT NULL,
    sender_stream           VARCHAR(32) NOT NULL,
    from_address            VARCHAR(255) NOT NULL,
    to_address              TEXT NOT NULL,
    provider_name           VARCHAR(64) NOT NULL,
    provider_message_id     VARCHAR(255),
    status                  VARCHAR(30) NOT NULL,
    failure_reason          TEXT,
    attempt_number          INT NOT NULL DEFAULT 1,
    attempted_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

ALTER TABLE delivery_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_attempts FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON delivery_attempts FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_delivery_attempts_intent
    ON delivery_attempts (tenant_id, message_intent_id);

CREATE INDEX IF NOT EXISTS idx_delivery_attempts_status
    ON delivery_attempts (tenant_id, status);

ALTER TABLE delivery_attempts
    ADD CONSTRAINT delivery_attempts_stream_known
    CHECK (sender_stream IN ('CRITICAL', 'TRANSACTIONAL', 'OPERATIONAL', 'MARKETING')) NOT VALID;

ALTER TABLE delivery_attempts
    ADD CONSTRAINT delivery_attempts_status_known
    CHECK (status IN ('QUEUED', 'ACCEPTED', 'FAILED', 'RETRYING')) NOT VALID;


-- 4. delivery_events: provider/mailbox delivery outcomes and webhook receipts
CREATE TABLE IF NOT EXISTS delivery_events (
    delivery_event_id       UUID PRIMARY KEY,
    provider_attempt_id     UUID NOT NULL REFERENCES delivery_attempts(provider_attempt_id) ON DELETE CASCADE,
    message_intent_id       UUID NOT NULL REFERENCES message_intents(message_intent_id) ON DELETE CASCADE,
    tenant_id               VARCHAR(255) NOT NULL,
    event_type              VARCHAR(32) NOT NULL,
    raw_payload             JSONB,
    occurred_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

ALTER TABLE delivery_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_events FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON delivery_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_delivery_events_attempt
    ON delivery_events (tenant_id, provider_attempt_id);

CREATE INDEX IF NOT EXISTS idx_delivery_events_intent
    ON delivery_events (tenant_id, message_intent_id);

ALTER TABLE delivery_events
    ADD CONSTRAINT delivery_events_type_known
    CHECK (event_type IN ('ACCEPTED', 'DELIVERED', 'BOUNCED', 'COMPLAINED', 'DROPPED')) NOT VALID;
