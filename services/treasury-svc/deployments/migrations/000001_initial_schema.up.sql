-- Initial Schema for Treasury & Cash Position Service

CREATE TABLE IF NOT EXISTS bank_accounts (
    bank_account_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(255) NOT NULL,
    legal_entity_id VARCHAR(255) NOT NULL,
    account_name VARCHAR(255) NOT NULL,
    masked_account_number VARCHAR(100) NOT NULL,
    bank_identifier VARCHAR(100) NOT NULL, -- BIC/SWIFT/routing
    currency_code VARCHAR(3) NOT NULL,
    account_status VARCHAR(50) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, SUSPENDED, CLOSED
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE bank_accounts ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON bank_accounts
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE IF NOT EXISTS cash_balances (
    balance_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(255) NOT NULL,
    bank_account_id UUID NOT NULL REFERENCES bank_accounts(bank_account_id) ON DELETE CASCADE,
    ledger_balance NUMERIC(20, 4) NOT NULL,
    available_balance NUMERIC(20, 4) NOT NULL,
    as_of_timestamp TIMESTAMP WITH TIME ZONE NOT NULL,
    correlation_id VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE cash_balances ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON cash_balances
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE IF NOT EXISTS liquidity_thresholds (
    threshold_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(255) NOT NULL,
    legal_entity_id VARCHAR(255) NOT NULL,
    currency_code VARCHAR(3) NOT NULL,
    minimum_required_balance NUMERIC(20, 4) NOT NULL,
    escalation_email VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE liquidity_thresholds ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON liquidity_thresholds
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));
