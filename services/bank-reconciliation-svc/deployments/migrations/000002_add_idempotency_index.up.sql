-- Migration: 000002_add_idempotency_index.up.sql
--
-- Adds a partial unique index on (tenant_id, correlation_id) so a retried
-- CreateStatementLine call (e.g. after a client-side timeout on a POST that
-- actually succeeded server-side) resolves to the ORIGINAL statement line
-- instead of creating a duplicate. Same pattern as general-ledger-svc's,
-- accounts-payable-svc's, accounts-receivable-svc's, and
-- purchase-request-svc's 000002 migrations.
--
-- Partial (WHERE correlation_id != '') because correlation_id predates this
-- migration as a plain NOT NULL column with no uniqueness requirement —
-- historical rows may share an empty string, which must never falsely
-- collide as a "duplicate" once this index exists.
CREATE UNIQUE INDEX idx_statement_lines_tenant_correlation
    ON statement_lines (tenant_id, correlation_id)
    WHERE correlation_id != '';
