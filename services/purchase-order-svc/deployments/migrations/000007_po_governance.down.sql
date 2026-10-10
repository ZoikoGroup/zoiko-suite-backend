-- Reverts 000007_po_governance.up.sql.
-- Rows created in DRAFT/PENDING_APPROVAL/APPROVED/ON_HOLD/CANCELLED remain (their
-- statuses are still allowed by the 000004 CHECK); only the governance objects go.

DROP TRIGGER IF EXISTS trg_po_guard_line ON purchase_order_lines;
DROP FUNCTION IF EXISTS po_guard_line();
DROP TRIGGER IF EXISTS trg_po_guard_order ON purchase_orders;
DROP FUNCTION IF EXISTS po_guard_order();
DROP TRIGGER IF EXISTS trg_po_progress_append_only ON purchase_order_progress;
DROP TRIGGER IF EXISTS trg_po_events_append_only ON purchase_order_events;
DROP TRIGGER IF EXISTS trg_po_revisions_append_only ON purchase_order_revisions;
DROP FUNCTION IF EXISTS po_reject_evidence_mutation();

ALTER TABLE purchase_order_lines
    DROP CONSTRAINT IF EXISTS purchase_order_lines_quantity_positive,
    DROP CONSTRAINT IF EXISTS purchase_order_lines_unit_price_nonneg,
    DROP CONSTRAINT IF EXISTS purchase_order_lines_amount_nonneg;

DROP INDEX IF EXISTS idx_purchase_orders_supplier;
DROP TABLE IF EXISTS idempotency_keys;
DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_order_events;
DROP TABLE IF EXISTS purchase_order_events;
DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_order_revisions;
DROP TABLE IF EXISTS purchase_order_revisions;

ALTER TABLE purchase_orders DROP CONSTRAINT IF EXISTS purchase_orders_held_from_check;
ALTER TABLE purchase_orders DROP CONSTRAINT IF EXISTS purchase_orders_approval_basis_check;
ALTER TABLE purchase_orders
    DROP COLUMN IF EXISTS payment_terms,
    DROP COLUMN IF EXISTS delivery_terms,
    DROP COLUMN IF EXISTS supplier_exception_by,
    DROP COLUMN IF EXISTS supplier_exception_ref,
    DROP COLUMN IF EXISTS held_from_status,
    DROP COLUMN IF EXISTS approval_ref,
    DROP COLUMN IF EXISTS approval_basis,
    DROP COLUMN IF EXISTS prepared_by_principal_id,
    DROP COLUMN IF EXISTS supplier_ref;

-- issued_by/issued_at were NOT NULL before: only restorable when no draft exists.
UPDATE purchase_orders SET issued_at = created_at WHERE issued_at IS NULL AND po_status IN ('ISSUED', 'CLOSED');
UPDATE purchase_orders SET issued_by_principal_id = 'unknown' WHERE issued_by_principal_id IS NULL AND po_status IN ('ISSUED', 'CLOSED');

-- restore 000006's row-level security on the outbox
ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON outbox_events
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
