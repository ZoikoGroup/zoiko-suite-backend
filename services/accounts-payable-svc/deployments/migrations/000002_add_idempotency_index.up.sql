-- Migration: 000002_add_idempotency_index.up.sql
--
-- Adds a partial unique index on (tenant_id, correlation_id) so a retried
-- CreateInvoice call (e.g. after a client-side timeout on a POST that
-- actually succeeded server-side) resolves to the ORIGINAL invoice instead
-- of creating a duplicate liability. Same pattern as general-ledger-svc's
-- 000002 migration.
--
-- Partial (WHERE correlation_id != '') because correlation_id predates this
-- migration as a plain NOT NULL column with no uniqueness requirement —
-- historical rows may share an empty string, which must never falsely
-- collide as a "duplicate" once this index exists.
CREATE UNIQUE INDEX idx_vendor_invoices_tenant_correlation
    ON vendor_invoices (tenant_id, correlation_id)
    WHERE correlation_id != '';
