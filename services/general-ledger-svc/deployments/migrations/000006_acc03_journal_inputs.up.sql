-- ACC-03 Journal Entry required business/source inputs
-- (ZS-ARCH-SVC-001 v2.0 §9.D).
--
-- The doc requires: "Book; entity; journal type; document/transaction date;
-- posting date; currency; debit/credit lines; accounts; dimensions;
-- description; source/evidence." Entity, lines, accounts, description and
-- source were already here. This migration adds the rest.
--
-- WHY SOME ARE NOT NULL AND OTHERS ARE NULLABLE
--
-- journal_type, transaction_date, posting_date and currency_code are facts the
-- business supplies and nothing can derive: a backdated accrual and a
-- same-day standard posting differ only in these fields, and a journal whose
-- currency is implied rather than stated cannot be translated later without
-- guessing. They are NOT NULL.
--
-- book_id and dimensions are nullable because the services that would issue
-- and validate them do not exist. REF-06 Accounting Book / Ledger Basis and
-- REF-08 Financial Dimension Registry are both unimplemented, so a NOT NULL
-- book_id would force every caller to invent an identifier nothing can check —
-- a posting claiming a basis nobody decided, which is what INV-03 exists to
-- prevent. Same posture as account_code (no Chart of Accounts) and
-- fiscal_calendar_id in tenant-entity-registry-svc: carry the field, state the
-- gap, tighten when the owner ships.

-- Backfill note: the DEFAULTs below exist only so the ALTER succeeds against
-- rows written before this migration. They are dropped immediately afterwards,
-- so no future insert can silently acquire a journal type or currency nobody
-- chose. Existing rows keep the backfilled values and are identifiable by
-- journal_type = 'UNSPECIFIED'.
ALTER TABLE journal_headers
    ADD COLUMN journal_type     VARCHAR(32)  NOT NULL DEFAULT 'UNSPECIFIED',
    ADD COLUMN transaction_date DATE         NOT NULL DEFAULT '1970-01-01',
    ADD COLUMN posting_date     DATE         NOT NULL DEFAULT '1970-01-01',
    ADD COLUMN currency_code    CHAR(3)      NOT NULL DEFAULT 'XXX',
    ADD COLUMN book_id          VARCHAR(64),
    ADD COLUMN reporting_basis  VARCHAR(32);

ALTER TABLE journal_headers
    ALTER COLUMN journal_type     DROP DEFAULT,
    ALTER COLUMN transaction_date DROP DEFAULT,
    ALTER COLUMN posting_date     DROP DEFAULT,
    ALTER COLUMN currency_code    DROP DEFAULT;

-- ACC-03 "dimensions". Per-line rather than per-header: a single journal
-- routinely splits one cost across cost centres or projects, and a header-level
-- dimension set could not express that.
--
-- JSONB rather than typed columns because REF-08 Financial Dimension Registry
-- does not exist — there is no authority saying which dimensions a tenant has,
-- so the shape cannot be fixed in DDL yet without inventing one.
ALTER TABLE journal_lines
    ADD COLUMN dimensions JSONB;

-- ACC-03 "source/evidence", and INV-10: material actions that require evidence
-- cannot complete without the corresponding evidence record. Carried from the
-- §4 envelope's evidence_refs so a posting keeps its substantiating documents.
ALTER TABLE journal_headers
    ADD COLUMN evidence_refs TEXT[];

-- Reporting reads the ledger by the date it hit the books, not by the date the
-- row was inserted; without this every period report is a sequential scan.
CREATE INDEX idx_journal_headers_posting_date
    ON journal_headers (tenant_id, legal_entity_id, posting_date);

CREATE INDEX idx_journal_headers_type
    ON journal_headers (tenant_id, journal_type);
