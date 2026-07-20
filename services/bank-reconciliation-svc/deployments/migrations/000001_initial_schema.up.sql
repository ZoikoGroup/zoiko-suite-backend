-- Migration: 000001_initial_schema.up.sql
-- Owned records for bank-reconciliation-svc.
-- Tenant-isolated via Postgres Row-Level Security AND explicit tenant_id
-- filters in every store query — RLS alone is insufficient given this
-- platform connects as a Postgres superuser (found via general-ledger-svc's
-- CI failure; see internal/store/pg_store.go's package doc comment).

CREATE TABLE statement_lines (
    statement_line_id       UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    legal_entity_id          UUID NOT NULL,
    bank_account_id          UUID NOT NULL,
    statement_date           DATE NOT NULL,
    amount                   NUMERIC(18,2) NOT NULL,
    currency_code            VARCHAR(3) NOT NULL,
    bank_reference           VARCHAR(255) NOT NULL,
    status                   VARCHAR(20) NOT NULL,

    matched_journal_id       UUID,
    matched_by_principal_id  VARCHAR(255),
    matched_at               TIMESTAMP WITH TIME ZONE,

    exception_reason         VARCHAR(500),
    flagged_by_principal_id  VARCHAR(255),
    flagged_at               TIMESTAMP WITH TIME ZONE,

    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_statement_lines_tenant ON statement_lines (tenant_id);
CREATE INDEX idx_statement_lines_status ON statement_lines (status);
-- Supports the statement-completion check: "any UNMATCHED lines left for
-- this bank account + statement date?"
CREATE INDEX idx_statement_lines_account_date ON statement_lines (tenant_id, bank_account_id, statement_date);

ALTER TABLE statement_lines ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON statement_lines
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
