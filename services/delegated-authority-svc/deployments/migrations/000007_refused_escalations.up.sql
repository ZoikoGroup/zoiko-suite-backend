-- Refused escalation attempts: durable record of refused delegation creation
-- attempts. ORG-06 §4.2: "Every refused escalation attempt leaves durable
-- evidence." This includes self-dealing, delegator mismatch, delegator lacks
-- authority, SoD conflict, overlap conflict, and invalid time window.

CREATE TABLE IF NOT EXISTS refused_escalations (
    refused_id         UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    legal_entity_id    VARCHAR(255) NOT NULL,
    caller_principal_id VARCHAR(255) NOT NULL,
    delegator_principal_id VARCHAR(255) NOT NULL,
    delegate_principal_id VARCHAR(255) NOT NULL,
    action_type        VARCHAR(100) NOT NULL,
    effective_from     TIMESTAMPTZ NOT NULL,
    effective_to       TIMESTAMPTZ NOT NULL,
    refusal_reason     VARCHAR(50) NOT NULL,  -- self_dealing, delegator_mismatch, delegator_lacks_authority, sod_conflict, overlap_conflict, invalid_window, no_create_grant
    correlation_id     VARCHAR(255),
    idempotency_key    VARCHAR(255),
    request_id         VARCHAR(255),
    source_channel     VARCHAR(50),
    refused_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT refused_escalations_reason_known
        CHECK (refusal_reason IN ('self_dealing', 'delegator_mismatch', 'delegator_lacks_authority', 'sod_conflict', 'overlap_conflict', 'invalid_window', 'no_create_grant'))
);

CREATE INDEX IF NOT EXISTS idx_refused_escalations_lookup
    ON refused_escalations (tenant_id, legal_entity_id, caller_principal_id, refused_at DESC);

CREATE INDEX IF NOT EXISTS idx_refused_escalations_delegator
    ON refused_escalations (tenant_id, delegator_principal_id, refused_at DESC);

ALTER TABLE refused_escalations ENABLE ROW LEVEL SECURITY;
ALTER TABLE refused_escalations FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS refused_escalations_tenant_isolation ON refused_escalations;
CREATE POLICY refused_escalations_tenant_isolation ON refused_escalations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));