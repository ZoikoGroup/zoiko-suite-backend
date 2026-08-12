-- Initial schema for Notification Service (notification-svc)

CREATE TABLE IF NOT EXISTS notifications (
    notification_id          UUID PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    legal_entity_id            VARCHAR(255) NOT NULL,
    recipient_principal_id     VARCHAR(255) NOT NULL,
    channel                    VARCHAR(20) NOT NULL, -- 'EMAIL', 'SMS', 'IN_APP', 'WEBHOOK'
    subject                    VARCHAR(255) NOT NULL,
    body                       TEXT NOT NULL,
    status                     VARCHAR(20) NOT NULL, -- 'PENDING', 'SENT', 'FAILED'
    source_event_type          VARCHAR(100),
    source_reference           VARCHAR(255),
    correlation_id             VARCHAR(255) NOT NULL,
    failure_reason             TEXT,
    created_by_principal_id     VARCHAR(255) NOT NULL,
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL,
    sent_at                    TIMESTAMP WITH TIME ZONE
);

-- Enable Row-Level Security
ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policy
CREATE POLICY tenant_isolation_policy ON notifications FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried send with the same (tenant_id, correlation_id) must
-- resolve to the original notification, never a duplicate send.
CREATE UNIQUE INDEX idx_notifications_tenant_correlation ON notifications (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_notifications_tenant_entity ON notifications (tenant_id, legal_entity_id);
CREATE INDEX idx_notifications_tenant_recipient ON notifications (tenant_id, recipient_principal_id);
CREATE INDEX idx_notifications_tenant_status ON notifications (tenant_id, status);
