-- 20260818001400_general_ledger_svc.sql
-- general-ledger-svc → schema `general_ledger`
--
-- Squashed end state of 000001_initial_schema, 000002_add_idempotency_index
-- and 000003_add_atomic_linking. Two tables: journal_headers, journal_lines.
--
-- No chart_of_accounts table: no Chart-of-Accounts service exists anywhere on
-- the platform, so account_code is a plain, unvalidated string — a documented
-- v1 gap, not an oversight.

CREATE SCHEMA IF NOT EXISTS general_ledger;

COMMENT ON SCHEMA general_ledger IS
    'general-ledger-svc. Double-entry journal headers and their append-only lines. Corrections are reversing journals, never edits.';

GRANT USAGE ON SCHEMA general_ledger TO zoiko_backend, authenticated;

-- ── journal_headers ──────────────────────────────────────────────────────────

CREATE TABLE general_ledger.journal_headers (
    journal_id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID         NOT NULL,
    legal_entity_id           UUID         NOT NULL,

    fiscal_period             VARCHAR(20)  NOT NULL,

    -- DRAFT | VALIDATED | POSTED | REVERSED
    status                    VARCHAR(20)  NOT NULL,

    -- The link from a reversing journal back to what it reverses. Without it a
    -- reversal is just another journal that happens to carry opposite signs,
    -- and nothing can state which entry corrected which.
    reversal_of_journal_id    UUID         REFERENCES general_ledger.journal_headers(journal_id),

    description               TEXT         NOT NULL,

    created_by_principal_id   VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),
    validated_by_principal_id VARCHAR(255),
    posted_by_principal_id    VARCHAR(255),
    reversed_by_principal_id  VARCHAR(255),

    -- Atomic Linking: carry the upstream event and the governing decision that
    -- caused this posting WHERE ONE EXISTS. Both nullable — a manually entered
    -- journal has neither, and Atomic Linking means "carry the link when there
    -- is one", not "fabricate one when there isn't".
    source_event_id           TEXT,
    governance_decision_id    TEXT,

    correlation_id            VARCHAR(255) NOT NULL,
    created_at                TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    validated_at              TIMESTAMPTZ,
    posted_at                 TIMESTAMPTZ,
    reversed_at               TIMESTAMPTZ,

    -- Each transition must carry its evidence. A POSTED journal with no
    -- posted_at or no posting principal is an entry in the books that nobody
    -- is recorded as having made.
    CONSTRAINT journal_headers_validated_has_evidence
        CHECK (validated_at IS NULL OR validated_by_principal_id IS NOT NULL),
    CONSTRAINT journal_headers_posted_has_evidence
        CHECK (posted_at IS NULL OR posted_by_principal_id IS NOT NULL),
    CONSTRAINT journal_headers_reversed_has_evidence
        CHECK (reversed_at IS NULL OR reversed_by_principal_id IS NOT NULL),

    -- A journal cannot reverse itself.
    CONSTRAINT journal_headers_not_self_reversal
        CHECK (reversal_of_journal_id IS NULL OR reversal_of_journal_id <> journal_id)
);

CREATE INDEX idx_journal_headers_tenant
    ON general_ledger.journal_headers (tenant_id);
CREATE INDEX idx_journal_headers_entity_period
    ON general_ledger.journal_headers (legal_entity_id, fiscal_period);
CREATE INDEX idx_journal_headers_status
    ON general_ledger.journal_headers (status);

-- Idempotency: a retried CreateJournal returns the ORIGINAL journal rather than
-- posting a second one. Partial because correlation_id was once optional and an
-- empty string must never collide across genuinely different journals.
CREATE UNIQUE INDEX idx_journal_headers_tenant_correlation
    ON general_ledger.journal_headers (tenant_id, correlation_id)
    WHERE correlation_id != '';

-- A journal may be reversed once. Without this, two concurrent reversals of the
-- same entry both succeed and the books are corrected twice — the non-atomic
-- reversal that double-counted.
CREATE UNIQUE INDEX idx_journal_headers_one_reversal_per_journal
    ON general_ledger.journal_headers (reversal_of_journal_id)
    WHERE reversal_of_journal_id IS NOT NULL;

-- Composite key journal_lines points at, so a line and its header always agree
-- on the tenant.
CREATE UNIQUE INDEX idx_journal_headers_id_tenant
    ON general_ledger.journal_headers (journal_id, tenant_id);

-- ── journal_lines ────────────────────────────────────────────────────────────
-- Append-only: lines are written once at journal creation and never updated.
-- Corrections happen only through a new reversing journal — no finalised
-- journal may be hard-edited.

CREATE TABLE general_ledger.journal_lines (
    journal_line_id       UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    journal_id            UUID          NOT NULL,
    tenant_id             UUID          NOT NULL,

    line_number           INTEGER       NOT NULL,
    account_code          VARCHAR(64)   NOT NULL,
    debit_amount          NUMERIC(18,2) NOT NULL DEFAULT 0,
    credit_amount         NUMERIC(18,2) NOT NULL DEFAULT 0,
    description           TEXT,

    -- A line with a tax basis carries the immutable snapshot it was computed
    -- from. No TaxLogicSnapshot-producing service exists on the platform yet,
    -- so this is always NULL in practice — a documented v1 gap.
    tax_code              VARCHAR(64),
    tax_logic_snapshot_id TEXT,

    UNIQUE (journal_id, line_number),

    CONSTRAINT journal_lines_line_number_positive
        CHECK (line_number >= 1),

    -- Amounts are magnitudes; direction is which column they land in.
    CONSTRAINT journal_lines_amounts_non_negative
        CHECK (debit_amount >= 0 AND credit_amount >= 0),

    -- Exactly one side. A line carrying both a debit and a credit, or neither,
    -- is not a double-entry line — it is two entries or none pretending to be
    -- one, and it makes the journal's balance unverifiable line by line.
    CONSTRAINT journal_lines_exactly_one_side
        CHECK ((debit_amount > 0) <> (credit_amount > 0)),

    -- Composite, so a line cannot be attached to another tenant's journal.
    CONSTRAINT journal_lines_header_fk
        FOREIGN KEY (journal_id, tenant_id)
        REFERENCES general_ledger.journal_headers (journal_id, tenant_id)
);

CREATE INDEX idx_journal_lines_journal ON general_ledger.journal_lines (journal_id);

-- NOTE ON BALANCE. That sum(debit) = sum(credit) for a journal is a
-- cross-row invariant, so it is not expressible as a CHECK. It is enforced by
-- the service before a journal is posted. Making the database the authority
-- would need a DEFERRABLE constraint trigger firing at COMMIT — worth doing,
-- but it is a behaviour change rather than a schema translation, so it is not
-- smuggled in here.

CREATE TRIGGER journal_lines_immutable
    BEFORE UPDATE OR DELETE ON general_ledger.journal_lines
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE general_ledger.journal_headers ENABLE ROW LEVEL SECURITY;
ALTER TABLE general_ledger.journal_headers FORCE  ROW LEVEL SECURITY;
ALTER TABLE general_ledger.journal_lines   ENABLE ROW LEVEL SECURITY;
ALTER TABLE general_ledger.journal_lines   FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON general_ledger.journal_headers
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON general_ledger.journal_headers
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_isolation ON general_ledger.journal_lines
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON general_ledger.journal_lines
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON general_ledger.journal_headers TO authenticated;
GRANT SELECT ON general_ledger.journal_lines   TO authenticated;

-- Headers transition (DRAFT → VALIDATED → POSTED → REVERSED), so UPDATE is
-- granted. Lines never change.
GRANT SELECT, INSERT, UPDATE ON general_ledger.journal_headers TO zoiko_backend;
GRANT SELECT, INSERT         ON general_ledger.journal_lines   TO zoiko_backend;
