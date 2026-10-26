-- 000019_accounting_kernel_posting.up.sql
--
-- Global Accounting Kernel, Wave 1 (posting core): the journal model of standard §8, atomic posting (§7.1), the
-- event-to-posting uniqueness record (§29), journal numbering (§36), posted-journal immutability (AK-INV, §42),
-- manual-journal controls (§9) and reversal lineage (§11).
--
-- Nothing here rewrites history: existing journals keep every value they have, and the new columns are nullable.

-- ── journal header: the fields §8.1 asks for that the header did not carry ───────────────────────────────────────────

ALTER TABLE journal_headers
    ADD COLUMN journal_number              VARCHAR(40),
    ADD COLUMN accounting_book_id          UUID,
    ADD COLUMN source_accounting_event_id  UUID,
    ADD COLUMN posting_execution_id        UUID,
    ADD COLUMN journal_hash                VARCHAR(80),
    ADD COLUMN journal_purpose             VARCHAR(40),
    ADD COLUMN correction_kind             VARCHAR(40),
    ADD COLUMN reversal_reason             TEXT,
    ADD COLUMN copy_of_journal_id          UUID REFERENCES journal_headers(journal_id),
    ADD COLUMN recurring_template_id       UUID,
    ADD COLUMN withdrawn_at                TIMESTAMPTZ,
    ADD COLUMN withdrawn_by_principal_id   VARCHAR(255),
    ADD COLUMN withdrawn_reason            TEXT;

-- The human-readable number is unique inside its scope (entity + book + year) and is never a relational key.
CREATE UNIQUE INDEX uq_journal_headers_number
    ON journal_headers (tenant_id, legal_entity_id, COALESCE(book_id, ''), journal_number)
    WHERE journal_number IS NOT NULL;

CREATE INDEX idx_journal_headers_source_event ON journal_headers (tenant_id, source_accounting_event_id)
    WHERE source_accounting_event_id IS NOT NULL;

ALTER TABLE journal_headers
    ADD CONSTRAINT journal_headers_correction_kind CHECK (correction_kind IS NULL OR correction_kind IN
        ('ERROR_CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'SOURCE_DOCUMENT_CORRECTION', 'COMMERCIAL_ADJUSTMENT', 'PERIOD_REOPEN_CORRECTION', 'MIGRATION_CORRECTION')),
    ADD CONSTRAINT journal_headers_purpose CHECK (journal_purpose IS NULL OR journal_purpose IN
        ('ACCRUAL', 'PREPAYMENT', 'RECLASSIFICATION', 'CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'OPENING_BALANCE', 'MIGRATION', 'CLOSE_ADJUSTMENT', 'ALLOCATION', 'REVALUATION', 'OTHER'));

-- ── journal line: transaction currency, rate and rule provenance (§8.2) ────────────────────────────────────────────

ALTER TABLE journal_lines
    ADD COLUMN transaction_amount     NUMERIC(28,8),
    ADD COLUMN transaction_currency   CHAR(3),
    ADD COLUMN exchange_rate          NUMERIC(28,12),
    ADD COLUMN rate_ref               VARCHAR(200),
    ADD COLUMN posting_rule_line_ref  VARCHAR(64);

-- ── journal numbering (§36): gap-free per scope, allocated in the posting transaction so a rollback gives it back ───

CREATE TABLE journal_number_counters (
    tenant_id        UUID         NOT NULL,
    legal_entity_id  UUID         NOT NULL,
    book_key         VARCHAR(64)  NOT NULL DEFAULT '',
    scope_year       INTEGER      NOT NULL,
    last_number      BIGINT       NOT NULL,
    PRIMARY KEY (tenant_id, legal_entity_id, book_key, scope_year)
);
ALTER TABLE journal_number_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE journal_number_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON journal_number_counters
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ── the posting execution: which rule produced it, from which event, and the ledger watermark it reached ────────────

ALTER TABLE posting_executions
    ADD COLUMN accounting_event_id   UUID,
    ADD COLUMN accounting_book_id    UUID,
    ADD COLUMN posting_basis         VARCHAR(40),
    ADD COLUMN posting_rule_set_id   UUID,
    ADD COLUMN rule_version          VARCHAR(40),
    ADD COLUMN proposal_hash         VARCHAR(80),
    ADD COLUMN ledger_watermark      BIGINT,
    ADD COLUMN journal_event_id      VARCHAR(80);

CREATE INDEX idx_posting_executions_event ON posting_executions (tenant_id, accounting_event_id)
    WHERE accounting_event_id IS NOT NULL;

-- ── event-to-posting uniqueness (§29): one consequence per accounting event + book + posting basis ─────────────────

CREATE TABLE event_posting_links (
    tenant_id             UUID         NOT NULL,
    accounting_event_id   UUID         NOT NULL REFERENCES accounting_events(accounting_event_id),
    accounting_book_id    UUID         NOT NULL REFERENCES accounting_books(book_id),
    posting_basis         VARCHAR(40)  NOT NULL DEFAULT 'PRIMARY',
    journal_id            UUID         NOT NULL REFERENCES journal_headers(journal_id),
    posting_execution_id  UUID         NOT NULL REFERENCES posting_executions(execution_id),
    posting_rule_set_id   UUID         NOT NULL REFERENCES posting_rule_sets(posting_rule_set_id),
    rule_version          VARCHAR(40)  NOT NULL,
    proposal_hash         VARCHAR(80)  NOT NULL,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, accounting_event_id, accounting_book_id, posting_basis)
);
CREATE INDEX idx_event_posting_links_journal ON event_posting_links (tenant_id, journal_id);
ALTER TABLE event_posting_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_posting_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON event_posting_links
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE TRIGGER event_posting_links_no_change BEFORE UPDATE OR DELETE ON event_posting_links
    FOR EACH ROW EXECUTE FUNCTION kernel_append_only();

-- ── chart of accounts: book applicability and currency restriction (§12) ──────────────────────────────────────────
-- NULL means "every book" / "any currency", which is what every existing account has.

ALTER TABLE chart_of_accounts
    ADD COLUMN applicable_book_ids   UUID[],
    ADD COLUMN currency_restriction  CHAR(3);

-- ── recurring journal templates (§9): a template is never itself posted, it generates new draft journals ───────────

CREATE TABLE recurring_journal_templates (
    template_id        UUID PRIMARY KEY,
    tenant_id          UUID          NOT NULL,
    legal_entity_id    UUID          NOT NULL,
    name               VARCHAR(160)  NOT NULL,
    journal_type       VARCHAR(32)   NOT NULL,
    journal_purpose    VARCHAR(40)   NOT NULL,
    currency_code      CHAR(3)       NOT NULL,
    book_id            VARCHAR(64),
    description        TEXT          NOT NULL,
    lines              JSONB         NOT NULL,
    frequency          VARCHAR(12)   NOT NULL,
    next_run_date      DATE          NOT NULL,
    anchor_day         SMALLINT      NOT NULL,
    end_date           DATE,
    status             VARCHAR(8)    NOT NULL DEFAULT 'ACTIVE',
    created_by         VARCHAR(255)  NOT NULL,
    created_at         TIMESTAMPTZ   NOT NULL DEFAULT now(),
    last_generated_at  TIMESTAMPTZ,
    CONSTRAINT recurring_frequency CHECK (frequency IN ('WEEKLY', 'MONTHLY', 'QUARTERLY', 'YEARLY')),
    CONSTRAINT recurring_status CHECK (status IN ('ACTIVE', 'PAUSED', 'ENDED')),
    CONSTRAINT recurring_name UNIQUE (tenant_id, legal_entity_id, name)
);
ALTER TABLE recurring_journal_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE recurring_journal_templates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON recurring_journal_templates
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ── posted journals are immutable (AK-INV, §42) ────────────────────────────────────────────────────────────────────
-- Journals posted before this migration are marked as such, so the line guard below treats them as sealed too.

UPDATE journal_headers SET journal_hash = 'PRE-KERNEL' WHERE status IN ('FINALIZED', 'REVERSED') AND journal_hash IS NULL;

CREATE OR REPLACE FUNCTION journal_header_posted_guard() RETURNS trigger AS $$
DECLARE
    allowed CONSTANT text[] := ARRAY['status', 'reversed_by_principal_id', 'reversed_at', 'approval_status', 'journal_number', 'journal_hash'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status IN ('FINALIZED', 'REVERSED') THEN
            RAISE EXCEPTION 'posted journal % cannot be deleted; correct it with a reversal', OLD.journal_id USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.status NOT IN ('FINALIZED', 'REVERSED') THEN
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - allowed) IS DISTINCT FROM (to_jsonb(OLD) - allowed) THEN
        RAISE EXCEPTION 'posted journal % is immutable; correct it with a reversal', OLD.journal_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'FINALIZED' AND NEW.status = 'REVERSED') THEN
        RAISE EXCEPTION 'posted journal % cannot move from % to %', OLD.journal_id, OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.journal_number IS NOT NULL AND NEW.journal_number IS DISTINCT FROM OLD.journal_number THEN
        RAISE EXCEPTION 'the number of journal % is fixed', OLD.journal_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.journal_hash IS NOT NULL AND NEW.journal_hash IS DISTINCT FROM OLD.journal_hash THEN
        RAISE EXCEPTION 'the hash of journal % is fixed', OLD.journal_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER journal_headers_posted_guard BEFORE UPDATE OR DELETE ON journal_headers
    FOR EACH ROW EXECUTE FUNCTION journal_header_posted_guard();

-- Lines: never changed or removed once the journal is posted, and none added once the journal is sealed (a journal born
-- posted — a reversal — gets its lines in the same transaction that seals it).
CREATE OR REPLACE FUNCTION journal_line_posted_guard() RETURNS trigger AS $$
DECLARE
    st text;
    sealed text;
    jid uuid;
BEGIN
    jid := CASE WHEN TG_OP = 'INSERT' THEN NEW.journal_id ELSE OLD.journal_id END;
    SELECT status, journal_hash INTO st, sealed FROM journal_headers WHERE journal_id = jid;
    IF st IS NULL THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF st IN ('FINALIZED', 'REVERSED') AND sealed IS NOT NULL THEN
            RAISE EXCEPTION 'posted journal % is immutable; no line may be added', jid USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF st IN ('FINALIZED', 'REVERSED') THEN
        RAISE EXCEPTION 'posted journal % is immutable; its lines cannot be %', jid, lower(TG_OP) USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER journal_lines_posted_guard BEFORE INSERT OR UPDATE OR DELETE ON journal_lines
    FOR EACH ROW EXECUTE FUNCTION journal_line_posted_guard();

-- ── superseding accounting events (§11, §29): a correction is a new event that names the one it corrects ─────────────
ALTER TABLE accounting_events ADD COLUMN supersedes_event_id UUID REFERENCES accounting_events(accounting_event_id);
CREATE INDEX accounting_events_supersedes ON accounting_events (tenant_id, supersedes_event_id) WHERE supersedes_event_id IS NOT NULL;
