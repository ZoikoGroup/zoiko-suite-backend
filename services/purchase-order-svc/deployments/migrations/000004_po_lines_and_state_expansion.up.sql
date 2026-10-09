-- Migration: 000004_po_lines_and_state_expansion.up.sql
--
-- Adds purchase order lines, expands po_status to full state machine,
-- adds tracking columns for approval/hold/cancel workflow.

-- 1. PO Lines table — line items with quantity, price, UOM, delivery terms
CREATE TABLE purchase_order_lines (
    line_id              UUID PRIMARY KEY,
    tenant_id            UUID NOT NULL,
    purchase_order_id    UUID NOT NULL REFERENCES purchase_orders(purchase_order_id),
    line_number          INTEGER NOT NULL,
    item_ref             TEXT,
    description          TEXT,
    quantity             NUMERIC(18,4) NOT NULL,
    unit_price           NUMERIC(18,4) NOT NULL,
    uom                  VARCHAR(20),
    line_amount          NUMERIC(18,2) NOT NULL,
    delivery_date        TIMESTAMPTZ,
    delivery_location    TEXT,
    received_quantity    NUMERIC(18,4) NOT NULL DEFAULT 0,
    invoiced_quantity    NUMERIC(18,4) NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_po_lines_order ON purchase_order_lines (purchase_order_id);
CREATE INDEX idx_po_lines_tenant ON purchase_order_lines (tenant_id);

-- Unique line_number per order
CREATE UNIQUE INDEX idx_po_lines_order_line_number ON purchase_order_lines (purchase_order_id, line_number);

-- 2. Expand po_status CHECK constraint to full state machine
ALTER TABLE purchase_orders DROP CONSTRAINT IF EXISTS purchase_orders_po_status_check;
ALTER TABLE purchase_orders ADD CONSTRAINT purchase_orders_po_status_check 
    CHECK (po_status IN ('DRAFT','PENDING_APPROVAL','APPROVED','ISSUED',
                         'PARTIALLY_RECEIVED','PARTIALLY_INVOICED','FULFILLED','CLOSED',
                         'ON_HOLD','CANCELLED'));

-- 3. Add columns for approval/hold/cancel workflow
ALTER TABLE purchase_orders 
    ADD COLUMN IF NOT EXISTS submitted_by_principal_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS approved_by_principal_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS hold_reason TEXT,
    ADD COLUMN IF NOT EXISTS held_by_principal_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS held_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancelled_by_principal_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancellation_reason TEXT,
    ADD COLUMN IF NOT EXISTS revision INTEGER NOT NULL DEFAULT 1;

-- 4. Default existing rows to ISSUED (they already are) with revision=1
UPDATE purchase_orders SET revision = 1 WHERE revision IS NULL;

-- 5. RLS for purchase_order_lines
ALTER TABLE purchase_order_lines ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_order_lines
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- 6. FORCE RLS on purchase_order_lines (same as purchase_orders)
ALTER TABLE purchase_order_lines FORCE ROW LEVEL SECURITY;