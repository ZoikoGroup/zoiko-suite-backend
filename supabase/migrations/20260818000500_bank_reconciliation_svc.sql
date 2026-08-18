-- 20260818000500_bank_reconciliation_svc.sql
-- bank-reconciliation-svc → schema `bank_reconciliation`
--
-- Squashed end state of 000001_initial_schema, 000002_add_idempotency_index
-- and 000003_add_gl_cash_account_code. One table: statement_lines.

CREATE SCHEMA IF NOT EXISTS bank_reconciliation;

COMMENT ON SCHEMA bank_reconciliation IS
    'bank-reconciliation-svc. Bank statement lines and their match / exception state against the general ledger.';

GRANT USAGE ON SCHEMA bank_reconciliation TO zoiko_backend, authenticated;

-- ── statement_lines ──────────────────────────────────────────────────────────

CREATE TABLE bank_reconciliation.statement_lines (
    statement_line_id       UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID          NOT NULL,
    legal_entity_id         UUID          NOT NULL,
    bank_account_id         UUID          NOT NULL,

    statement_date          DATE          NOT NULL,
    amount                  NUMERIC(18,2) NOT NULL,
    currency_code           VARCHAR(3)    NOT NULL,
    bank_reference          VARCHAR(255)  NOT NULL,

    -- UNMATCHED | MATCHED | EXCEPTION
    status                  VARCHAR(20)   NOT NULL,

    -- Which general-ledger account represents this bank account. Required to
    -- verify the DIRECTION of a match, and that is not a nicety:
    --
    -- Matching used to compare magnitudes — abs(journal net) against
    -- abs(statement amount). A journal's net was the sum of one side of a
    -- balanced double-entry journal, always positive, carrying no direction at
    -- all. So a statement line of -500.00 (money leaving the account)
    -- reconciled cleanly against a journal that moved 500.00 IN. A payment out
    -- matching a receipt in is precisely the error, or the concealment, that
    -- reconciling exists to surface.
    --
    -- Direction cannot be recovered from the journal alone — it depends on
    -- which side the CASH line falls on, so the account code IS the missing
    -- fact. Nullable because nothing on the platform maps a bank_account_id to
    -- a ledger account code; a NULL is NOT "skip the check", the handler
    -- refuses to match such a line at all with a distinct error.
    gl_cash_account_code    VARCHAR(50),

    matched_journal_id      UUID,
    matched_by_principal_id VARCHAR(255),
    matched_at              TIMESTAMPTZ,

    exception_reason        VARCHAR(500),
    flagged_by_principal_id VARCHAR(255),
    flagged_at              TIMESTAMPTZ,

    correlation_id          VARCHAR(255)  NOT NULL,
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    -- Terminal states carry their evidence. New here for the same reason as
    -- the other services in this migration set: an empty database has no
    -- backlog that would force these to be added NOT VALID.
    CONSTRAINT statement_lines_matched_has_evidence
        CHECK ((status = 'MATCHED') = (matched_at IS NOT NULL AND matched_journal_id IS NOT NULL)),
    CONSTRAINT statement_lines_exception_has_reason
        CHECK (status <> 'EXCEPTION' OR (exception_reason IS NOT NULL AND exception_reason <> ''))
);

COMMENT ON COLUMN bank_reconciliation.statement_lines.gl_cash_account_code IS
    'general-ledger account code representing this bank account. Required to verify the direction of a match; a line without one cannot be matched.';

CREATE INDEX idx_statement_lines_tenant ON bank_reconciliation.statement_lines (tenant_id);
CREATE INDEX idx_statement_lines_status ON bank_reconciliation.statement_lines (status);

-- Serves the statement-completion check: "any UNMATCHED lines left for this
-- bank account + statement date?"
CREATE INDEX idx_statement_lines_account_date
    ON bank_reconciliation.statement_lines (tenant_id, bank_account_id, statement_date);

-- Idempotency: a retried ingest resolves to the ORIGINAL line.
CREATE UNIQUE INDEX idx_statement_lines_tenant_correlation
    ON bank_reconciliation.statement_lines (tenant_id, correlation_id)
    WHERE correlation_id != '';

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE bank_reconciliation.statement_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_reconciliation.statement_lines FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON bank_reconciliation.statement_lines
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON bank_reconciliation.statement_lines
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON bank_reconciliation.statement_lines TO authenticated;
GRANT SELECT, INSERT, UPDATE ON bank_reconciliation.statement_lines TO zoiko_backend;
