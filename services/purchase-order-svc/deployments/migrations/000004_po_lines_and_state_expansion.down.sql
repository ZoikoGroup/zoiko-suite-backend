-- Migration: 000004_po_lines_and_state_expansion.down.sql
--
-- Reverts 000004_po_lines_and_state_expansion.up.sql

-- Drop RLS policy and disable RLS
DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_order_lines;
ALTER TABLE purchase_order_lines DISABLE ROW LEVEL SECURITY;

-- Drop columns from purchase_orders
ALTER TABLE purchase_orders 
    DROP COLUMN IF EXISTS submitted_by_principal_id,
    DROP COLUMN IF EXISTS submitted_at,
    DROP COLUMN IF EXISTS approved_by_principal_id,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS hold_reason,
    DROP COLUMN IF EXISTS held_by_principal_id,
    DROP COLUMN IF EXISTS held_at,
    DROP COLUMN IF EXISTS cancelled_by_principal_id,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS cancellation_reason,
    DROP COLUMN IF EXISTS revision;

-- Restore original po_status CHECK constraint
ALTER TABLE purchase_orders DROP CONSTRAINT IF EXISTS purchase_orders_po_status_check;
ALTER TABLE purchase_orders ADD CONSTRAINT purchase_orders_po_status_check 
    CHECK (po_status IN ('ISSUED','CLOSED'));

-- Drop purchase_order_lines table
DROP INDEX IF EXISTS idx_po_lines_order;
DROP INDEX IF EXISTS idx_po_lines_tenant;
DROP INDEX IF EXISTS idx_po_lines_order_line_number;
DROP TABLE IF EXISTS purchase_order_lines;