-- Migration: 000001_initial_schema.up.sql
--
-- Owned records for purchase-request-svc per docs/architecture/03-microservices.md
-- §12.8: purchase request headers. Tenant-isolated via Postgres Row-Level
-- Security, matching accounts-payable-svc's pattern.
--
-- RLS here is defense-in-depth, not the sole isolation guarantee — this
-- platform's services connect as a Postgres superuser, which unconditionally
-- bypasses RLS. Every query in this service's store layer filters explicitly
-- by tenant_id in its own SQL for that reason.

CREATE TABLE purchase_requests (
    request_id                 UUID PRIMARY KEY,
    tenant_id                  UUID NOT NULL,
    legal_entity_id            UUID NOT NULL,
    requested_by_principal_id  VARCHAR(255) NOT NULL,
    description                TEXT NOT NULL,
    amount                     NUMERIC(18,2) NOT NULL,
    currency_code              VARCHAR(3) NOT NULL,
    status                     VARCHAR(20) NOT NULL,
    approved_by_principal_id   VARCHAR(255),
    rejected_by_principal_id   VARCHAR(255),
    rejection_reason           TEXT,
    correlation_id             VARCHAR(255) NOT NULL,
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    approved_at                TIMESTAMP WITH TIME ZONE,
    rejected_at                TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_purchase_requests_tenant ON purchase_requests (tenant_id);
CREATE INDEX idx_purchase_requests_entity ON purchase_requests (legal_entity_id);
CREATE INDEX idx_purchase_requests_status ON purchase_requests (status);

ALTER TABLE purchase_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_requests
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
