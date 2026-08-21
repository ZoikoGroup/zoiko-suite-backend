-- 0005_purchase_request_svc.sql
-- purchase-request-svc → schema `purchase_request`
--
-- Squashed end state of 000001_initial_schema and 000002_add_idempotency_index.
-- One table: purchase_requests.

CREATE SCHEMA IF NOT EXISTS purchase_request;

COMMENT ON SCHEMA purchase_request IS
    'purchase-request-svc. Purchase request headers through the create → approve/reject lifecycle.';

GRANT USAGE ON SCHEMA purchase_request TO zoiko_backend, authenticated;

-- ── purchase_requests ────────────────────────────────────────────────────────

CREATE TABLE purchase_request.purchase_requests (
    request_id                UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID          NOT NULL,
    legal_entity_id           UUID          NOT NULL,

    -- The requester is the verified caller, not a body field.
    requested_by_principal_id VARCHAR(255)  NOT NULL DEFAULT app.current_principal_id(),

    description               TEXT          NOT NULL,
    amount                    NUMERIC(18,2) NOT NULL,
    currency_code             VARCHAR(3)    NOT NULL,

    -- PENDING | APPROVED | REJECTED
    status                    VARCHAR(20)   NOT NULL,

    approved_by_principal_id  VARCHAR(255),
    rejected_by_principal_id  VARCHAR(255),
    rejection_reason          TEXT,

    correlation_id            VARCHAR(255)  NOT NULL,
    created_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    approved_at               TIMESTAMPTZ,
    rejected_at               TIMESTAMPTZ,

    -- A terminal state must carry its evidence, and must not carry the other
    -- state's. Not present on the compose estate, where the table has real
    -- history that could not be guaranteed to satisfy it; an empty database
    -- has no such backlog. Same reasoning as the delegated-authority pass.
    CONSTRAINT purchase_requests_approved_has_evidence
        CHECK ((status = 'APPROVED') = (approved_at IS NOT NULL AND approved_by_principal_id IS NOT NULL)),
    CONSTRAINT purchase_requests_rejected_has_evidence
        CHECK ((status = 'REJECTED') = (rejected_at IS NOT NULL AND rejected_by_principal_id IS NOT NULL))
);

CREATE INDEX idx_purchase_requests_tenant ON purchase_request.purchase_requests (tenant_id);
CREATE INDEX idx_purchase_requests_entity ON purchase_request.purchase_requests (legal_entity_id);
CREATE INDEX idx_purchase_requests_status ON purchase_request.purchase_requests (status);

-- Idempotency: a retried CreateRequest resolves to the ORIGINAL request rather
-- than creating a duplicate. Partial for the same reason as accounts-payable's.
CREATE UNIQUE INDEX idx_purchase_requests_tenant_correlation
    ON purchase_request.purchase_requests (tenant_id, correlation_id)
    WHERE correlation_id != '';

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE purchase_request.purchase_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_request.purchase_requests FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON purchase_request.purchase_requests
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON purchase_request.purchase_requests
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON purchase_request.purchase_requests TO authenticated;
GRANT SELECT, INSERT, UPDATE ON purchase_request.purchase_requests TO zoiko_backend;
