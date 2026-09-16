DROP TABLE IF EXISTS treasury_transfers CASCADE;

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
