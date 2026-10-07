-- Migration: 000011_add_posted_ledger.up.sql
--
-- ACC-05 (General Ledger): "owns Posted ledger entries/balances. Must
-- never own: Draft journals or source workflow." Fuller ownership:
-- "LedgerEntry and authoritative posted balance state; no draft/manual
-- business lifecycle." State model: "Append-only committed entries;
-- balance projections versioned/rebuildable from entries."
--
-- ACC-04's own migration (000009) reasoned that journal_headers/
-- journal_lines already served as ACC-05's storage, so no separate
-- table was built then. Building ACC-05 for real now exposes why that
-- was only a bootstrap simplification, not the finished authority: the
-- same journal_headers row holds a journal from DRAFT through FINALIZED
-- — meaning the "draft journals" ACC-05 must never own and the "posted
-- ledger entries" it must own were the same mutable row. That is a real
-- authority-matrix violation once ACC-05 is asked to stand on its own,
-- not just a naming gap. This migration corrects it: ledger_entries is
-- a genuinely separate, physically append-only table, populated ONLY as
-- a side effect of a journal's PENDING/VALIDATED -> FINALIZED transition
-- (see PgStore.TransitionJournal, extended in the same migration's Go
-- change to call appendLedgerEntries inside the same DB transaction as
-- the status flip — so no call site can finalize a journal without also
-- appending its entries, the same whack-a-mole bug class already hit
-- twice this session with MarkJournalPosted).
--
-- Negative-path coverage (Minimum Negative-Path Acceptance table):
--  1. "Direct DB/API ledger write" -> no route accepts a ledger_entries
--     write; AppendPostedJournal is "(internal only)" per the spec's own
--     wireframe and is never registered as an HTTP route. A trigger
--     additionally rejects any UPDATE/DELETE at the database level, so
--     even a future handler bug cannot mutate a committed entry.
--  2. "Duplicate journal append" -> UNIQUE(tenant_id, journal_id,
--     journal_line_id) with ON CONFLICT DO NOTHING makes the append
--     idempotent; combined with TransitionJournal's own guarded
--     WHERE status = $fromStatus, a journal can only ever be finalized
--     (and therefore appended) once.
--  3. "Balance projection corrupt while entries intact" ->
--     RebuildDerivedBalanceProjection recomputes ledger_balances from
--     ledger_entries alone and never touches ledger_entries itself.
--  4. "Cross-book query leakage" -> every query is tenant-RLS-scoped and
--     additionally filtered by legal_entity_id/book_id in application code.

CREATE TABLE ledger_entries (
    ledger_entry_id     UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    legal_entity_id     UUID NOT NULL,
    book_id             VARCHAR(255) NOT NULL DEFAULT '',
    fiscal_period       VARCHAR(20) NOT NULL,
    journal_id          UUID NOT NULL,
    journal_line_id     UUID NOT NULL,
    line_number         INTEGER NOT NULL,
    account_code        VARCHAR(64) NOT NULL,
    debit_amount        NUMERIC(18,2) NOT NULL DEFAULT 0,
    credit_amount       NUMERIC(18,2) NOT NULL DEFAULT 0,
    currency_code       VARCHAR(3) NOT NULL,
    -- Nullable, matching journal_lines.dimensions' own posture (migration
    -- 000006): no dimension registry exists, so a line with none is a real,
    -- ordinary state, not an omission.
    dimensions          JSONB,
    transaction_date    DATE NOT NULL,
    posting_date        DATE NOT NULL,
    source_event_id     VARCHAR(255),
    correlation_id      VARCHAR(255) NOT NULL,
    entry_seq           BIGSERIAL,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT uq_ledger_entries_journal_line UNIQUE (tenant_id, journal_id, journal_line_id)
);

ALTER TABLE ledger_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON ledger_entries
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

CREATE INDEX idx_ledger_entries_scope ON ledger_entries (tenant_id, legal_entity_id, book_id, account_code, fiscal_period);
CREATE INDEX idx_ledger_entries_journal ON ledger_entries (tenant_id, journal_id);
CREATE INDEX idx_ledger_entries_source_event ON ledger_entries (tenant_id, source_event_id) WHERE source_event_id IS NOT NULL;

-- Physical append-only enforcement: no UPDATE or DELETE, ever, on this
-- table, regardless of which code path or future handler attempts it.
CREATE OR REPLACE FUNCTION reject_ledger_entry_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_ledger_entries_no_update
    BEFORE UPDATE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_entry_mutation();

CREATE TRIGGER trg_ledger_entries_no_delete
    BEFORE DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_entry_mutation();

-- Rebuildable balance projection — "balance projections versioned/
-- rebuildable from entries." Never hand-mutated; only ever replaced
-- wholesale by RebuildDerivedBalanceProjection for a given scope.
CREATE TABLE ledger_balances (
    tenant_id            UUID NOT NULL,
    legal_entity_id      UUID NOT NULL,
    book_id              VARCHAR(255) NOT NULL DEFAULT '',
    account_code         VARCHAR(64) NOT NULL,
    fiscal_period        VARCHAR(20) NOT NULL,
    dimensions_key       TEXT NOT NULL DEFAULT '{}',
    debit_total          NUMERIC(18,2) NOT NULL DEFAULT 0,
    credit_total         NUMERIC(18,2) NOT NULL DEFAULT 0,
    net_balance          NUMERIC(18,2) NOT NULL DEFAULT 0,
    watermark_entry_seq  BIGINT NOT NULL DEFAULT 0,
    rebuilt_at           TIMESTAMP WITH TIME ZONE NOT NULL,

    PRIMARY KEY (tenant_id, legal_entity_id, book_id, account_code, fiscal_period, dimensions_key)
);

ALTER TABLE ledger_balances ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_balances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON ledger_balances
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
