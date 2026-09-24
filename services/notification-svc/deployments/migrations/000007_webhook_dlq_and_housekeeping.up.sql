-- 000007_webhook_dlq_and_housekeeping.up.sql
-- Webhook Dead Letter Queue (DLQ) and Housekeeping Platform Scope Policies (ZS-COMMS-EMAIL-001 v2.0 §4, §13, §14)

-- 1. webhook_dlq: Dead letter queue for malformed, unresolvable, or repeatedly failing provider webhooks
CREATE TABLE IF NOT EXISTS webhook_dlq (
    dlq_id              UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    provider_name       VARCHAR(64) NOT NULL,
    event_type          VARCHAR(64),
    raw_payload         JSONB NOT NULL,
    error_reason        TEXT NOT NULL,
    is_retryable        BOOLEAN NOT NULL DEFAULT false,
    retry_count         INT NOT NULL DEFAULT 0,
    next_retry_at       TIMESTAMP WITH TIME ZONE,
    status              VARCHAR(30) NOT NULL DEFAULT 'FAILED',
    received_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    last_processed_at   TIMESTAMP WITH TIME ZONE
);

ALTER TABLE webhook_dlq ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_dlq FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON webhook_dlq;
CREATE POLICY tenant_isolation_policy ON webhook_dlq FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS platform_scope_read_policy ON webhook_dlq;
CREATE POLICY platform_scope_read_policy ON webhook_dlq
    FOR SELECT
    USING (current_setting('app.platform_scope', true) = 'true');

CREATE INDEX IF NOT EXISTS idx_webhook_dlq_tenant_status
    ON webhook_dlq (tenant_id, status);

CREATE INDEX IF NOT EXISTS idx_webhook_dlq_retry
    ON webhook_dlq (status, next_retry_at)
    WHERE is_retryable = true;

-- 2. Platform scope read policies for cross-tenant discovery by housekeeping workers
DROP POLICY IF EXISTS platform_scope_read_policy ON action_tokens;
CREATE POLICY platform_scope_read_policy ON action_tokens
    FOR SELECT
    USING (current_setting('app.platform_scope', true) = 'true');

DROP POLICY IF EXISTS platform_scope_read_policy ON message_intents;
CREATE POLICY platform_scope_read_policy ON message_intents
    FOR SELECT
    USING (current_setting('app.platform_scope', true) = 'true');

DROP POLICY IF EXISTS platform_scope_read_policy ON delivery_attempts;
CREATE POLICY platform_scope_read_policy ON delivery_attempts
    FOR SELECT
    USING (current_setting('app.platform_scope', true) = 'true');

-- 3. Indexes for efficient housekeeping and webhook provider lookups
CREATE INDEX IF NOT EXISTS idx_action_tokens_expired_active
    ON action_tokens (expires_at)
    WHERE status = 'ACTIVE';

CREATE INDEX IF NOT EXISTS idx_action_tokens_terminal_purge
    ON action_tokens (tenant_id, status, created_at)
    WHERE status IN ('EXPIRED', 'CONSUMED', 'REVOKED');

CREATE INDEX IF NOT EXISTS idx_message_intents_stale_cleanup
    ON message_intents (tenant_id, status, created_at);

CREATE INDEX IF NOT EXISTS idx_delivery_attempts_provider_msg
    ON delivery_attempts (provider_message_id)
    WHERE provider_message_id IS NOT NULL;
