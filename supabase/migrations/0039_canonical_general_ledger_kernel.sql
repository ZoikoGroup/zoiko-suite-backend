-- 0039_canonical_general_ledger_kernel.sql
-- ZoikoSuite Canonical General Ledger Kernel & Accounting Event Bridge
-- (ZS-DATA-001 §11, Invariants D05, D06, D08)
--
-- DOCTRINE:
-- 1. Tri-phase commit / immutable posted financial facts: No hard-edits or soft-deletes.
--    Corrections are performed strictly via reversal journals (reversal_of_journal_entry_id).
-- 2. Exact decimal arithmetic: All monetary debit/credit amounts use NUMERIC(38,12).
-- 3. Double-entry integrity: Balanced journals with non-zero lines.
-- 4. Multi-book awareness: Posted against explicit legal_entity_id, accounting_book_id, and ledger_id.

CREATE SCHEMA IF NOT EXISTS general_ledger;

COMMENT ON SCHEMA general_ledger IS
    'Canonical General Ledger Kernel. Accounting events, journal entries, balanced journal lines, and certified ledger balance snapshots.';

GRANT USAGE ON SCHEMA general_ledger TO zoiko_backend, authenticated;

-- ── accounting_events (ACC-EVENT) ─────────────────────────────────────────────

CREATE TABLE general_ledger.accounting_events (
    accounting_event_id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    source_domain                       VARCHAR(30)   NOT NULL, -- 'AR' | 'AP' | 'PAYROLL' | 'ASSETS' | 'TAX' | 'TREASURY'
    source_object_table                 VARCHAR(50)   NOT NULL, -- e.g. 'sales_invoices', 'supplier_invoices'
    source_object_id                    UUID          NOT NULL,
    event_type                          VARCHAR(50)   NOT NULL, -- 'INVOICE_ISSUED', 'PAYMENT_APPLIED', etc.
    occurred_at                         TIMESTAMPTZ   NOT NULL,
    effective_date                      DATE          NOT NULL,
    amount_basis                        JSONB         NOT NULL, -- Exact source figures
    posting_policy_version              VARCHAR(50)   NOT NULL,
    status                              VARCHAR(20)   NOT NULL DEFAULT 'PENDING', -- 'PENDING' | 'POSTED' | 'REJECTED' | 'REVERSED'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_acc_event_status CHECK (status IN ('PENDING', 'POSTED', 'REJECTED', 'REVERSED'))
);

CREATE INDEX idx_accounting_events_tenant_source ON general_ledger.accounting_events (tenant_id, source_domain, source_object_id);
CREATE INDEX idx_accounting_events_tenant_status ON general_ledger.accounting_events (tenant_id, status);

ALTER TABLE general_ledger.accounting_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE general_ledger.accounting_events FORCE ROW LEVEL SECURITY;

CREATE POLICY accounting_events_tenant_isolation ON general_ledger.accounting_events
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON general_ledger.accounting_events TO zoiko_backend, authenticated;

-- ── journal_entries (ACC-JE) ──────────────────────────────────────────────────

CREATE TABLE general_ledger.journal_entries (
    journal_entry_id                    UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    accounting_book_id                  UUID          NOT NULL,
    ledger_id                           UUID          NOT NULL,
    fiscal_period_id                    UUID          NOT NULL,
    journal_number                      VARCHAR(50)   NOT NULL,
    journal_type                        VARCHAR(30)   NOT NULL, -- 'STANDARD' | 'ADJUSTMENT' | 'REVERSAL' | 'CLOSING' | 'ELIMINATION' | 'ACCRUAL'
    posting_date                        DATE          NOT NULL,
    source_accounting_event_id          UUID,
    reversal_of_journal_entry_id        UUID,
    status                              VARCHAR(20)   NOT NULL DEFAULT 'DRAFT', -- 'DRAFT' | 'APPROVED' | 'POSTED' | 'REVERSED'
    posted_at                           TIMESTAMPTZ,
    posted_by                           VARCHAR(255),
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_je_type CHECK (journal_type IN ('STANDARD', 'ADJUSTMENT', 'REVERSAL', 'CLOSING', 'ELIMINATION', 'ACCRUAL')),
    CONSTRAINT chk_je_status CHECK (status IN ('DRAFT', 'APPROVED', 'POSTED', 'REVERSED')),
    CONSTRAINT fk_je_source_event FOREIGN KEY (source_accounting_event_id) REFERENCES general_ledger.accounting_events(accounting_event_id),
    CONSTRAINT fk_je_reversal FOREIGN KEY (reversal_of_journal_entry_id) REFERENCES general_ledger.journal_entries(journal_entry_id)
);

CREATE INDEX idx_journal_entries_tenant_ledger ON general_ledger.journal_entries (tenant_id, ledger_id, fiscal_period_id);
CREATE INDEX idx_journal_entries_tenant_posting_date ON general_ledger.journal_entries (tenant_id, posting_date);
CREATE INDEX idx_journal_entries_tenant_status ON general_ledger.journal_entries (tenant_id, status);

ALTER TABLE general_ledger.journal_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE general_ledger.journal_entries FORCE ROW LEVEL SECURITY;

CREATE POLICY journal_entries_tenant_isolation ON general_ledger.journal_entries
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON general_ledger.journal_entries TO zoiko_backend, authenticated;

-- ── journal_lines (ACC-JL) ───────────────────────────────────────────────────

CREATE TABLE general_ledger.journal_lines (
    journal_line_id                     UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    journal_entry_id                    UUID          NOT NULL,
    line_no                             INT           NOT NULL,
    account_id                          UUID          NOT NULL,
    transaction_currency                VARCHAR(3)    NOT NULL,
    transaction_amount                  NUMERIC(38,12) NOT NULL,
    functional_currency                 VARCHAR(3)    NOT NULL,
    functional_amount                   NUMERIC(38,12) NOT NULL,
    debit_amount                        NUMERIC(38,12) NOT NULL DEFAULT 0.000000000000,
    credit_amount                       NUMERIC(38,12) NOT NULL DEFAULT 0.000000000000,
    counterparty_party_id               UUID,
    tax_component_id                    UUID,
    source_line_ref                     VARCHAR(100),
    description                         TEXT,

    CONSTRAINT uq_journal_line_seq UNIQUE (journal_entry_id, line_no),
    CONSTRAINT chk_jl_amounts_positive CHECK (debit_amount >= 0 AND credit_amount >= 0),
    CONSTRAINT chk_jl_mutual_exclusive CHECK (
        (debit_amount > 0 AND credit_amount = 0) OR
        (credit_amount > 0 AND debit_amount = 0)
    ),
    CONSTRAINT fk_jl_entry FOREIGN KEY (journal_entry_id) REFERENCES general_ledger.journal_entries(journal_entry_id) ON DELETE CASCADE
);

CREATE INDEX idx_journal_lines_tenant_account ON general_ledger.journal_lines (tenant_id, account_id);
CREATE INDEX idx_journal_lines_tenant_counterparty ON general_ledger.journal_lines (tenant_id, counterparty_party_id);

ALTER TABLE general_ledger.journal_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE general_ledger.journal_lines FORCE ROW LEVEL SECURITY;

CREATE POLICY journal_lines_tenant_isolation ON general_ledger.journal_lines
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON general_ledger.journal_lines TO zoiko_backend, authenticated;

-- ── ledger_balance_snapshots (ACC-SNAP) ───────────────────────────────────────

CREATE TABLE general_ledger.ledger_balance_snapshots (
    balance_snapshot_id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    ledger_id                           UUID          NOT NULL,
    fiscal_period_id                    UUID          NOT NULL,
    account_id                          UUID          NOT NULL,
    as_of_posting_watermark             TIMESTAMPTZ   NOT NULL,
    opening_balance                     NUMERIC(38,12) NOT NULL,
    period_debit_total                  NUMERIC(38,12) NOT NULL,
    period_credit_total                 NUMERIC(38,12) NOT NULL,
    closing_balance                     NUMERIC(38,12) NOT NULL,
    snapshot_hash                       VARCHAR(64)   NOT NULL, -- SHA-256
    status                              VARCHAR(20)   NOT NULL DEFAULT 'GENERATED', -- 'GENERATED' | 'CERTIFIED' | 'SUPERSEDED'
    generated_at                        TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    certified_by                        VARCHAR(255),
    certification_ref                   UUID,

    CONSTRAINT chk_snap_status CHECK (status IN ('GENERATED', 'CERTIFIED', 'SUPERSEDED')),
    CONSTRAINT uq_ledger_snap_account UNIQUE (tenant_id, ledger_id, fiscal_period_id, account_id)
);

CREATE INDEX idx_ledger_snapshots_tenant_period ON general_ledger.ledger_balance_snapshots (tenant_id, ledger_id, fiscal_period_id);

ALTER TABLE general_ledger.ledger_balance_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE general_ledger.ledger_balance_snapshots FORCE ROW LEVEL SECURITY;

CREATE POLICY ledger_snapshots_tenant_isolation ON general_ledger.ledger_balance_snapshots
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON general_ledger.ledger_balance_snapshots TO zoiko_backend, authenticated;
