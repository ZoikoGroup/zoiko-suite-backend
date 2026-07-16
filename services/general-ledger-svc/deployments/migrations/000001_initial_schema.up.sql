-- Migration: 000001_initial_schema.up.sql
--
-- Owned records for general-ledger-svc per docs/architecture/03-microservices.md
-- §10.1: journal headers, journal lines. Tenant-isolated via Postgres Row-Level
-- Security, matching tenant-entity-registry-svc's pattern (this is financial
-- data — RLS is not optional here).
--
-- No chart_of_accounts table: no Chart-of-Accounts service exists yet
-- anywhere in this platform, so account_code is a plain, unvalidated string
-- column — a documented v1 gap, not an oversight.

CREATE TABLE journal_headers (
    journal_id                  UUID PRIMARY KEY,
    tenant_id                   UUID NOT NULL,
    legal_entity_id             UUID NOT NULL,
    fiscal_period                VARCHAR(20) NOT NULL,
    status                       VARCHAR(20) NOT NULL,
    reversal_of_journal_id       UUID REFERENCES journal_headers(journal_id),
    description                  TEXT NOT NULL,
    created_by_principal_id      VARCHAR(255) NOT NULL,
    validated_by_principal_id    VARCHAR(255),
    posted_by_principal_id       VARCHAR(255),
    reversed_by_principal_id     VARCHAR(255),
    correlation_id                VARCHAR(255) NOT NULL,
    created_at                   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    validated_at                 TIMESTAMP WITH TIME ZONE,
    posted_at                    TIMESTAMP WITH TIME ZONE,
    reversed_at                  TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_journal_headers_tenant ON journal_headers (tenant_id);
CREATE INDEX idx_journal_headers_entity_period ON journal_headers (legal_entity_id, fiscal_period);
CREATE INDEX idx_journal_headers_status ON journal_headers (status);

ALTER TABLE journal_headers ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON journal_headers
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

-- Append-only: journal lines are written once at journal creation and never
-- updated. Corrections happen only via a brand-new reversing journal
-- (critical constraint — no finalized journal may be hard-edited).
CREATE TABLE journal_lines (
    journal_line_id  UUID PRIMARY KEY,
    journal_id       UUID NOT NULL REFERENCES journal_headers(journal_id),
    tenant_id        UUID NOT NULL,
    line_number      INTEGER NOT NULL,
    account_code     VARCHAR(64) NOT NULL,
    debit_amount     NUMERIC(18,2) NOT NULL DEFAULT 0,
    credit_amount    NUMERIC(18,2) NOT NULL DEFAULT 0,
    description      TEXT,

    UNIQUE (journal_id, line_number)
);

CREATE INDEX idx_journal_lines_journal ON journal_lines (journal_id);

ALTER TABLE journal_lines ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON journal_lines
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
