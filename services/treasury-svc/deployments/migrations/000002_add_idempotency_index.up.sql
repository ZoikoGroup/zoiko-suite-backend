-- Migration: 000002_add_idempotency_index.up.sql
--
-- ExecuteTransfer moves funds by inserting TWO cash_balances rows (debit
-- leg and credit leg) that deliberately SHARE one correlation_id — so a
-- uniqueness constraint on cash_balances itself would reject a transfer's
-- own second leg. Idempotency instead lives on a new transfers table: one
-- row per transfer INTENT, keyed by (tenant_id, correlation_id). A retried
-- InitiateTransfer call (e.g. a client timeout on a POST that actually
-- succeeded server-side) hits this table's unique index first and is
-- rejected as a no-op BEFORE either cash_balances leg is written — without
-- this, a retry would double-debit the source account and double-credit
-- the target, a real duplicate-money-movement bug.
CREATE TABLE IF NOT EXISTS transfers (
    transfer_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(255) NOT NULL,
    source_bank_account_id UUID NOT NULL,
    target_bank_account_id UUID NOT NULL,
    amount NUMERIC(20, 4) NOT NULL,
    currency_code VARCHAR(3) NOT NULL,
    correlation_id VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE transfers ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON transfers
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE UNIQUE INDEX idx_transfers_tenant_correlation
    ON transfers (tenant_id, correlation_id)
    WHERE correlation_id != '';
