-- Migration: 000005_progress_tracking.up.sql
--
-- Adds purchase_order_progress table for tracking received/invoiced quantities
-- pushed from goods-service-receipt-svc (AP-04) and accounts-payable-svc (AP-05).
-- Idempotent on (tenant_id, source_ref, kind).

CREATE TABLE purchase_order_progress (
    progress_id          UUID PRIMARY KEY,
    tenant_id            UUID NOT NULL,
    purchase_order_id    UUID NOT NULL REFERENCES purchase_orders(purchase_order_id),
    line_id              UUID NOT NULL REFERENCES purchase_order_lines(line_id),
    kind                 VARCHAR(20) NOT NULL CHECK (kind IN ('RECEIVED','INVOICED')),
    quantity             NUMERIC(18,4) NOT NULL,
    amount               NUMERIC(18,2) NOT NULL,
    source_ref           TEXT NOT NULL,
    delta_sign           INTEGER NOT NULL CHECK (delta_sign IN (1,-1)),
    correlation_id       TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_po_progress_order ON purchase_order_progress (purchase_order_id);
CREATE INDEX idx_po_progress_line ON purchase_order_progress (line_id);

-- Idempotency: one progress record per source_ref+kind per tenant
CREATE UNIQUE INDEX idx_po_progress_idempotency 
    ON purchase_order_progress (tenant_id, source_ref, kind);

-- RLS
ALTER TABLE purchase_order_progress ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_order_progress
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE purchase_order_progress FORCE ROW LEVEL SECURITY;