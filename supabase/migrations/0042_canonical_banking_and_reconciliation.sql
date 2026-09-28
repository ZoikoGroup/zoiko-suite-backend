-- 0042_canonical_banking_and_reconciliation.sql
-- ZoikoSuite Canonical Banking, Secure Connections & Reconciliation Matching
-- (ZS-DATA-001 §12, Invariants D14, D15)
--
-- DOCTRINE:
-- 1. Sensitive Data Masking (Invariant D14): Raw account numbers and IBANs are referenced via
--    vault tokens; presentation columns carry masked strings (e.g. '****1234').
-- 2. Reconciliation Matching (Invariant D15): Bank transactions link to finalized GL journals
--    or payments through attributable reconciliation match records.
-- 3. Exact Precision: Amounts use NUMERIC(38,12) exact arithmetic.

CREATE SCHEMA IF NOT EXISTS banking_cash;

COMMENT ON SCHEMA banking_cash IS
    'Canonical Banking & Cash Management. Bank connections, statements, transactions, and reconciliation matches.';

GRANT USAGE ON SCHEMA banking_cash TO zoiko_backend, authenticated;

-- ── bank_connections (BNK-CONN) ───────────────────────────────────────────────

CREATE TABLE banking_cash.bank_connections (
    bank_connection_id                  UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    institution_id                      VARCHAR(100)  NOT NULL,
    account_number_vault_ref            TEXT          NOT NULL, -- Encrypted vault token
    masked_account_number               VARCHAR(30)   NOT NULL, -- e.g. '********1234'
    iban_vault_ref                      TEXT,
    masked_iban                         VARCHAR(40),
    currency_code                       VARCHAR(3)    NOT NULL,
    status                              VARCHAR(20)   NOT NULL DEFAULT 'ACTIVE', -- 'ACTIVE' | 'DISCONNECTED' | 'CONSENT_REQUIRED'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_conn_status CHECK (status IN ('ACTIVE', 'DISCONNECTED', 'CONSENT_REQUIRED'))
);

CREATE INDEX idx_bank_conn_tenant_entity ON banking_cash.bank_connections (tenant_id, legal_entity_id, status);

ALTER TABLE banking_cash.bank_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE banking_cash.bank_connections FORCE ROW LEVEL SECURITY;

CREATE POLICY bank_conn_tenant_isolation ON banking_cash.bank_connections
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON banking_cash.bank_connections TO zoiko_backend, authenticated;

-- ── bank_statements (BNK-STMT) ────────────────────────────────────────────────

CREATE TABLE banking_cash.bank_statements (
    bank_statement_id                   UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    bank_connection_id                  UUID          NOT NULL,
    statement_number                    VARCHAR(100)  NOT NULL,
    statement_date                      DATE          NOT NULL,
    opening_balance                     NUMERIC(38,12) NOT NULL,
    closing_balance                     NUMERIC(38,12) NOT NULL,
    currency_code                       VARCHAR(3)    NOT NULL,
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT fk_stmt_conn FOREIGN KEY (bank_connection_id) REFERENCES banking_cash.bank_connections(bank_connection_id) ON DELETE RESTRICT
);

CREATE INDEX idx_bank_stmt_tenant_conn ON banking_cash.bank_statements (tenant_id, bank_connection_id, statement_date);

ALTER TABLE banking_cash.bank_statements ENABLE ROW LEVEL SECURITY;
ALTER TABLE banking_cash.bank_statements FORCE ROW LEVEL SECURITY;

CREATE POLICY bank_stmt_tenant_isolation ON banking_cash.bank_statements
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON banking_cash.bank_statements TO zoiko_backend, authenticated;

-- ── bank_transactions (BNK-TXN) ───────────────────────────────────────────────

CREATE TABLE banking_cash.bank_transactions (
    bank_transaction_id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    bank_statement_id                   UUID          NOT NULL,
    transaction_date                    DATE          NOT NULL,
    value_date                          DATE          NOT NULL,
    amount                              NUMERIC(38,12) NOT NULL, -- Positive = credit/inflow, Negative = debit/outflow
    currency_code                       VARCHAR(3)    NOT NULL,
    bank_reference                      TEXT          NOT NULL,
    counterparty_name                   VARCHAR(255),
    match_status                        VARCHAR(20)   NOT NULL DEFAULT 'UNMATCHED', -- 'UNMATCHED' | 'MATCHED' | 'EXCEPTION' | 'RESOLVED'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_bank_tx_match_status CHECK (match_status IN ('UNMATCHED', 'MATCHED', 'EXCEPTION', 'RESOLVED')),
    CONSTRAINT fk_tx_stmt FOREIGN KEY (bank_statement_id) REFERENCES banking_cash.bank_statements(bank_statement_id) ON DELETE CASCADE
);

CREATE INDEX idx_bank_tx_tenant_match ON banking_cash.bank_transactions (tenant_id, match_status);
CREATE INDEX idx_bank_tx_tenant_dates ON banking_cash.bank_transactions (tenant_id, transaction_date);

ALTER TABLE banking_cash.bank_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE banking_cash.bank_transactions FORCE ROW LEVEL SECURITY;

CREATE POLICY bank_tx_tenant_isolation ON banking_cash.bank_transactions
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON banking_cash.bank_transactions TO zoiko_backend, authenticated;

-- ── bank_reconciliation_matches (BNK-MATCH) ───────────────────────────────────

CREATE TABLE banking_cash.bank_reconciliation_matches (
    match_id                            UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    bank_transaction_id                 UUID          NOT NULL,
    matched_object_type                 VARCHAR(50)   NOT NULL, -- 'journal_entry' | 'cash_receipt' | 'payment_execution'
    matched_object_id                   UUID          NOT NULL,
    matched_amount                      NUMERIC(38,12) NOT NULL,
    difference_amount                   NUMERIC(38,12) NOT NULL DEFAULT 0.000000000000,
    matched_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    matched_by                          VARCHAR(255)  NOT NULL, -- 'RULE_ENGINE' | principal

    CONSTRAINT fk_match_tx FOREIGN KEY (bank_transaction_id) REFERENCES banking_cash.bank_transactions(bank_transaction_id) ON DELETE CASCADE
);

CREATE INDEX idx_recon_matches_tenant_tx ON banking_cash.bank_reconciliation_matches (tenant_id, bank_transaction_id);
CREATE INDEX idx_recon_matches_tenant_object ON banking_cash.bank_reconciliation_matches (tenant_id, matched_object_type, matched_object_id);

ALTER TABLE banking_cash.bank_reconciliation_matches ENABLE ROW LEVEL SECURITY;
ALTER TABLE banking_cash.bank_reconciliation_matches FORCE ROW LEVEL SECURITY;

CREATE POLICY recon_matches_tenant_isolation ON banking_cash.bank_reconciliation_matches
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON banking_cash.bank_reconciliation_matches TO zoiko_backend, authenticated;
