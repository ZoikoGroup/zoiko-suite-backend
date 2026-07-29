-- Migration: 000001_initial_schema.up.sql
--
-- Owned records for purchase-order-svc per docs/architecture/03-microservices.md
-- §12.9: purchase-order issuance, amendment, and fulfillment-linked state.
-- Tenant-isolated via Postgres Row-Level Security, matching
-- purchase-request-svc's pattern.
--
-- RLS here is defense-in-depth, not the sole isolation guarantee — this
-- platform's services connect as a Postgres superuser, which unconditionally
-- bypasses RLS. Every query in this service's store layer filters explicitly
-- by tenant_id in its own SQL for that reason.

CREATE SEQUENCE purchase_order_number_seq;

CREATE TABLE purchase_orders (
    purchase_order_id           UUID PRIMARY KEY,
    tenant_id                   UUID NOT NULL,
    legal_entity_id             UUID NOT NULL,
    purchase_request_id         UUID,
    vendor_profile_id           UUID,
    po_number                   VARCHAR(32) NOT NULL,
    po_status                   VARCHAR(20) NOT NULL,
    total_amount                NUMERIC(18,2) NOT NULL,
    currency_code               VARCHAR(3) NOT NULL,
    version                     INTEGER NOT NULL DEFAULT 1,
    issued_by_principal_id      VARCHAR(255) NOT NULL,
    closed_by_principal_id      VARCHAR(255),
    correlation_id              VARCHAR(255) NOT NULL,
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    issued_at                   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    closed_at                   TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_purchase_orders_tenant ON purchase_orders (tenant_id);
CREATE INDEX idx_purchase_orders_entity ON purchase_orders (legal_entity_id);
CREATE INDEX idx_purchase_orders_status ON purchase_orders (po_status);
CREATE INDEX idx_purchase_orders_request ON purchase_orders (purchase_request_id);

-- Idempotency: a retried Issue with the same (tenant_id, correlation_id)
-- must return the original order, never mint a second one.
CREATE UNIQUE INDEX idx_purchase_orders_tenant_correlation ON purchase_orders (tenant_id, correlation_id);

-- po_number is human-facing and must be unique per tenant, not globally —
-- two tenants may legitimately land on the same sequence value.
CREATE UNIQUE INDEX idx_purchase_orders_tenant_po_number ON purchase_orders (tenant_id, po_number);

ALTER TABLE purchase_orders ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_orders
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

-- Append-only amendment ledger — never UPDATEd or DELETEd. One row per
-- amendment, preserving the full before/after value rather than
-- overwriting total_amount destructively (doctrine: no soft-delete, no
-- destructive overwrite of material history).
CREATE TABLE purchase_order_amendments (
    amendment_id             UUID PRIMARY KEY,
    purchase_order_id        UUID NOT NULL REFERENCES purchase_orders(purchase_order_id),
    tenant_id                UUID NOT NULL,
    from_version              INTEGER NOT NULL,
    to_version                INTEGER NOT NULL,
    previous_total_amount    NUMERIC(18,2) NOT NULL,
    new_total_amount         NUMERIC(18,2) NOT NULL,
    reason                   TEXT NOT NULL,
    amended_by_principal_id  VARCHAR(255) NOT NULL,
    amended_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_po_amendments_order ON purchase_order_amendments (purchase_order_id);
CREATE INDEX idx_po_amendments_tenant ON purchase_order_amendments (tenant_id);

ALTER TABLE purchase_order_amendments ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_order_amendments
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
