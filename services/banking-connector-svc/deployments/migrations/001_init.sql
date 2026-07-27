CREATE TABLE IF NOT EXISTS bank_connections (
    connection_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    bank_name VARCHAR(255) NOT NULL,
    bic VARCHAR(32),
    account_number VARCHAR(128) NOT NULL,
    currency VARCHAR(16) NOT NULL DEFAULT 'USD',
    status VARCHAR(32) NOT NULL DEFAULT 'CONNECTED',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_bank_connections_tenant_entity ON bank_connections(tenant_id, legal_entity_id);

CREATE TABLE IF NOT EXISTS bank_statements (
    statement_id VARCHAR(64) PRIMARY KEY,
    connection_id VARCHAR(64) NOT NULL REFERENCES bank_connections(connection_id) ON DELETE CASCADE,
    tenant_id VARCHAR(64) NOT NULL,
    statement_format VARCHAR(64) NOT NULL,
    statement_date TIMESTAMPTZ NOT NULL,
    opening_balance NUMERIC(18, 4) NOT NULL DEFAULT 0.0,
    closing_balance NUMERIC(18, 4) NOT NULL DEFAULT 0.0,
    transaction_count INT NOT NULL DEFAULT 0,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_bank_statements_conn_date ON bank_statements(connection_id, statement_date);
