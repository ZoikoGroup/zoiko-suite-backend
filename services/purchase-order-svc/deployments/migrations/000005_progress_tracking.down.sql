-- Migration: 000005_progress_tracking.down.sql
--
-- Reverts 000005_progress_tracking.up.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_order_progress;
ALTER TABLE purchase_order_progress DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS idx_po_progress_order;
DROP INDEX IF EXISTS idx_po_progress_line;
DROP INDEX IF EXISTS idx_po_progress_idempotency;
DROP TABLE IF EXISTS purchase_order_progress;