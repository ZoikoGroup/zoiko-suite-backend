-- Migration: 000002_add_idempotency_index.up.sql
--
-- Adds a partial unique index on (tenant_id, correlation_id) so a retried
-- CreateRequest call (e.g. after a client-side timeout on a POST that
-- actually succeeded server-side) resolves to the ORIGINAL request instead
-- of creating a duplicate. Same pattern as general-ledger-svc's,
-- accounts-payable-svc's, and accounts-receivable-svc's 000002 migrations.
--
-- Partial (WHERE correlation_id != '') because correlation_id predates this
-- migration as a plain NOT NULL column with no uniqueness requirement —
-- historical rows may share an empty string, which must never falsely
-- collide as a "duplicate" once this index exists.
CREATE UNIQUE INDEX idx_purchase_requests_tenant_correlation
    ON purchase_requests (tenant_id, correlation_id)
    WHERE correlation_id != '';
