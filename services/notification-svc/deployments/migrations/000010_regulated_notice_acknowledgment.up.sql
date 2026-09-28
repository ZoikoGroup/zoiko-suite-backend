-- NCD-05: Regulated Notice & Acknowledgment Chain.
--
-- §6 Regulated notices: mandatory delivery with legal acknowledgment chain.
-- NCD-05 has three surfaces: POST /v1/regulated-notices, POST /acknowledgements,
-- GET /acknowledgements, and a chain table for the evidence trail.

CREATE TABLE IF NOT EXISTS regulated_notices (
    regulated_notice_id  UUID PRIMARY KEY,
    tenant_id            VARCHAR(255) NOT NULL,
    legal_entity_id      VARCHAR(255) NOT NULL,
    intent_id            UUID REFERENCES communication_intents(intent_id) ON DELETE SET NULL,
    template_id          UUID REFERENCES templates(template_id) ON DELETE SET NULL,
    recipient_principal_id VARCHAR(255) NOT NULL,
    recipient_address    TEXT,                   -- snapshot at send time
    subject              TEXT NOT NULL,
    body                 TEXT NOT NULL,
    variables            JSONB NOT NULL DEFAULT '{}',
    channel              VARCHAR(20) NOT NULL,   -- EMAIL, IN_APP, WEBHOOK
    status               VARCHAR(30) NOT NULL DEFAULT 'pending', -- pending, sent, acknowledged, expired, failed
    priority             VARCHAR(20) NOT NULL DEFAULT 'normal', -- normal, high, critical
    expires_at           TIMESTAMPTZ,            -- deadline for acknowledgment
    acknowledged_at      TIMESTAMPTZ,
    acknowledged_by      VARCHAR(255),           -- who acknowledged
    acknowledgment_method VARCHAR(30),           -- click, digital_signature, witness, api
    acknowledgment_chain JSONB,                  -- full chain of evidence
    created_by           VARCHAR(255) NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT regulated_notices_status_known
        CHECK (status IN ('pending', 'sent', 'acknowledged', 'expired', 'failed')),
    CONSTRAINT regulated_notices_priority_known
        CHECK (priority IN ('normal', 'high', 'critical')),
    CONSTRAINT regulated_notices_channel_known
        CHECK (channel IN ('EMAIL', 'SMS', 'IN_APP', 'WEBHOOK'))
);

CREATE INDEX IF NOT EXISTS idx_regulated_notices_lookup
    ON regulated_notices (tenant_id, legal_entity_id, recipient_principal_id, status);

CREATE INDEX IF NOT EXISTS idx_regulated_notices_expires
    ON regulated_notices (expires_at) WHERE expires_at IS NOT NULL AND status IN ('pending', 'sent');

ALTER TABLE regulated_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE regulated_notices FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS regulated_notices_tenant_isolation ON regulated_notices;
CREATE POLICY regulated_notices_tenant_isolation ON regulated_notices FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Acknowledgment chain: immutable log of every action on the regulated notice
CREATE TABLE IF NOT EXISTS acknowledgment_chain (
    chain_id             UUID PRIMARY KEY,
    regulated_notice_id  UUID NOT NULL REFERENCES regulated_notices(regulated_notice_id) ON DELETE CASCADE,
    tenant_id            VARCHAR(255) NOT NULL,
    step_number          INT NOT NULL,
    action               VARCHAR(30) NOT NULL,   -- created, sent, delivered, opened, acknowledged, expired, failed
    actor                VARCHAR(255),           -- who performed the action (principal_id or 'system')
    method               VARCHAR(30),            -- how: email, in_app, api, digital_signature, witness
    evidence             JSONB,                  -- cryptographic proof, IP, user agent, signature, etc.
    metadata             JSONB,                  -- additional context
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT acknowledgment_chain_action_known
        CHECK (action IN ('created', 'sent', 'delivered', 'opened', 'acknowledged', 'expired', 'failed'))
);

CREATE INDEX IF NOT EXISTS idx_acknowledgment_chain_notice
    ON acknowledgment_chain (regulated_notice_id, step_number);

ALTER TABLE acknowledgment_chain ENABLE ROW LEVEL SECURITY;
ALTER TABLE acknowledgment_chain FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS acknowledgment_chain_tenant_isolation ON acknowledgment_chain;
CREATE POLICY acknowledgment_chain_tenant_isolation ON acknowledgment_chain FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));