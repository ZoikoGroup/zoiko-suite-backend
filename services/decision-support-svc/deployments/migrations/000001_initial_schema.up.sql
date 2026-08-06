-- Initial schema for Decision Support Service (decision-support-svc)

CREATE TABLE IF NOT EXISTS recommendations (
    recommendation_id          UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    legal_entity_id            VARCHAR(255) NOT NULL,
    subject_type               VARCHAR(100) NOT NULL,
    subject_reference          VARCHAR(255) NOT NULL,
    action_type                VARCHAR(100) NOT NULL,
    recommended_action         VARCHAR(20) NOT NULL, -- APPROVE, REJECT, ESCALATE, NO_HISTORY
    confidence_score           NUMERIC(4,3) NOT NULL CHECK (confidence_score >= 0 AND confidence_score <= 1),
    rationale                  TEXT NOT NULL,
    prior_decisions_sampled    INTEGER NOT NULL DEFAULT 0,
    requested_by_principal_id  VARCHAR(255) NOT NULL,
    correlation_id             VARCHAR(255) NOT NULL,
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE recommendations ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policy
CREATE POLICY tenant_isolation_policy ON recommendations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried request with the same (tenant_id, correlation_id)
-- must resolve to the original recommendation, never a duplicate.
CREATE UNIQUE INDEX idx_recommendations_tenant_correlation ON recommendations (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_recommendations_tenant_entity_subject ON recommendations (tenant_id, legal_entity_id, subject_reference);
