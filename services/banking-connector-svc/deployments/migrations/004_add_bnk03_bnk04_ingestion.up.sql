-- BNK-03 Statement Ingestion + BNK-04 Transaction Normalization, colocated
-- in this service because normalization must synchronously read BNK-03's
-- immutable raw evidence in the same transaction as recording supersession
-- lineage (same reasoning as the Audit domain's AUD-03/AUD-04 colocation).
--
-- bank_statements gains real ingestion-lifecycle state instead of being a
-- header-only, immediately-final record. content_hash makes a re-uploaded
-- statement idempotent rather than double-imported.

ALTER TABLE bank_statements
    ADD COLUMN content_hash VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN source_id VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN import_batch_id VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN status VARCHAR(32) NOT NULL DEFAULT 'ACCEPTED',
    ADD COLUMN quarantine_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN created_by_principal_id VARCHAR(128) NOT NULL DEFAULT '';

-- Idempotency: a re-uploaded statement (same connection, same bytes) must
-- return the original import, never create a duplicate one.
CREATE UNIQUE INDEX idx_bank_statements_conn_hash
    ON bank_statements (connection_id, content_hash) WHERE content_hash <> '';

-- bank_statement_lines: the actual line items BNK-03's header-only record
-- never had. Immutable once written — a statement is evidence, not a
-- draft to be edited.
CREATE TABLE bank_statement_lines (
    line_id           VARCHAR(64) PRIMARY KEY DEFAULT gen_random_uuid()::text,
    statement_id      VARCHAR(64) NOT NULL REFERENCES bank_statements (statement_id),
    tenant_id         VARCHAR(64) NOT NULL,
    line_seq          INT NOT NULL,
    posted_date       TIMESTAMPTZ NOT NULL,
    amount            NUMERIC(18, 4) NOT NULL,
    currency          VARCHAR(16) NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    raw_reference     TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (statement_id, line_seq)
);

CREATE INDEX idx_bank_statement_lines_statement ON bank_statement_lines (statement_id, line_seq);

ALTER TABLE bank_statement_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_statement_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_statement_lines
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE OR REPLACE FUNCTION reject_statement_line_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'bank_statement_lines rows are append-only evidence and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_statement_line_mutation
    BEFORE UPDATE OR DELETE ON bank_statement_lines
    FOR EACH ROW EXECUTE FUNCTION reject_statement_line_mutation();

-- An ACCEPTED (or QUARANTINED/REJECTED) statement header is also terminal —
-- only RECEIVED->VALIDATING->{ACCEPTED,QUARANTINED,REJECTED} is legal, and
-- once out of RECEIVED/VALIDATING the header cannot be rewritten. Existing
-- rows created before this migration default to ACCEPTED (backward
-- compatible: RecordStatement's old header-only behavior still works and
-- is now simply the terminal ACCEPTED state directly).
CREATE OR REPLACE FUNCTION reject_terminal_statement_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_statements rows are never deleted';
    END IF;
    IF OLD.status NOT IN ('RECEIVED', 'VALIDATING') THEN
        RAISE EXCEPTION 'bank statement % is % and can no longer be modified', OLD.statement_id, OLD.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_terminal_statement_mutation
    BEFORE UPDATE OR DELETE ON bank_statements
    FOR EACH ROW EXECUTE FUNCTION reject_terminal_statement_mutation();

-- BNK-04 Transaction Normalization: canonical transactions derived from
-- BNK-03's raw lines. Never edited in place — a correction supersedes the
-- prior version via superseded_by, preserving the full lineage.
CREATE TABLE bank_transactions_canonical (
    transaction_id     VARCHAR(64) PRIMARY KEY DEFAULT gen_random_uuid()::text,
    tenant_id          VARCHAR(64) NOT NULL,
    statement_line_id  VARCHAR(64) NOT NULL REFERENCES bank_statement_lines (line_id),
    mapping_version    INT NOT NULL DEFAULT 1,
    transaction_date   TIMESTAMPTZ NOT NULL,
    amount             NUMERIC(18, 4) NOT NULL,
    currency           VARCHAR(16) NOT NULL,
    category           VARCHAR(64) NOT NULL DEFAULT '',
    counterparty       VARCHAR(255) NOT NULL DEFAULT '',
    status             VARCHAR(32) NOT NULL DEFAULT 'NORMALIZED',
    -- DEFERRABLE INITIALLY DEFERRED: a supersession must mark the OLD row
    -- SUPERSEDED (pointing at the not-yet-inserted NEW row's id) before
    -- the NEW row exists, so this FK's check has to wait until commit.
    superseded_by      VARCHAR(64) NULL REFERENCES bank_transactions_canonical (transaction_id) DEFERRABLE INITIALLY DEFERRED,
    created_by_principal_id VARCHAR(128) NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_bank_txn_canonical_line ON bank_transactions_canonical (statement_line_id);
CREATE INDEX idx_bank_txn_canonical_tenant_status ON bank_transactions_canonical (tenant_id, status);

-- At most one non-superseded canonical transaction per statement line at
-- any time — a correction must supersede the current one, never coexist
-- with it.
CREATE UNIQUE INDEX idx_bank_txn_canonical_active_line
    ON bank_transactions_canonical (statement_line_id) WHERE status <> 'SUPERSEDED';

ALTER TABLE bank_transactions_canonical ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_transactions_canonical FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_transactions_canonical
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Immutable except the one controlled field a supersession writes:
-- superseded_by, and only while transitioning it from NULL to a value on a
-- row that is itself still NORMALIZED or QUARANTINED (never re-superseding
-- an already-superseded row, and never editing amount/category/etc after
-- the fact — that must go through a brand new canonical row instead).
CREATE OR REPLACE FUNCTION reject_canonical_txn_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_transactions_canonical rows are never deleted';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'canonical transaction % is already SUPERSEDED and can never be modified again', OLD.transaction_id;
    END IF;
    IF NEW.status <> 'SUPERSEDED'
        OR OLD.superseded_by IS NOT NULL
        OR NEW.transaction_id IS DISTINCT FROM OLD.transaction_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.statement_line_id IS DISTINCT FROM OLD.statement_line_id
        OR NEW.mapping_version IS DISTINCT FROM OLD.mapping_version
        OR NEW.transaction_date IS DISTINCT FROM OLD.transaction_date
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.category IS DISTINCT FROM OLD.category
        OR NEW.counterparty IS DISTINCT FROM OLD.counterparty
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'canonical transaction % only permits a NULL->value supersession write, not a field edit', OLD.transaction_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_canonical_txn_mutation
    BEFORE UPDATE OR DELETE ON bank_transactions_canonical
    FOR EACH ROW EXECUTE FUNCTION reject_canonical_txn_mutation();

-- mapping_exceptions: raised when normalization can't confidently map a
-- line (unknown code, ambiguous sign) rather than guessing. Maker-checker:
-- one principal raises it (implicitly, by quarantining), a different
-- principal must approve its resolution.
CREATE TABLE mapping_exceptions (
    exception_id        VARCHAR(64) PRIMARY KEY DEFAULT gen_random_uuid()::text,
    tenant_id            VARCHAR(64) NOT NULL,
    statement_line_id    VARCHAR(64) NOT NULL REFERENCES bank_statement_lines (line_id),
    reason               TEXT NOT NULL,
    status               VARCHAR(32) NOT NULL DEFAULT 'OPEN',
    raised_by_principal_id    VARCHAR(128) NOT NULL,
    resolved_transaction_id   VARCHAR(64) NULL REFERENCES bank_transactions_canonical (transaction_id),
    resolved_by_principal_id  VARCHAR(128) NOT NULL DEFAULT '',
    resolved_at          TIMESTAMPTZ NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_mapping_exceptions_tenant_status ON mapping_exceptions (tenant_id, status);

-- At most one OPEN exception per statement line at a time.
CREATE UNIQUE INDEX idx_mapping_exceptions_open_line
    ON mapping_exceptions (statement_line_id) WHERE status = 'OPEN';

ALTER TABLE mapping_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE mapping_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON mapping_exceptions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Resolved exceptions (APPROVED/REJECTED) are terminal.
CREATE OR REPLACE FUNCTION reject_resolved_exception_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'mapping_exceptions rows are never deleted';
    END IF;
    IF OLD.status <> 'OPEN' THEN
        RAISE EXCEPTION 'mapping exception % is % and can no longer be modified', OLD.exception_id, OLD.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_resolved_exception_mutation
    BEFORE UPDATE OR DELETE ON mapping_exceptions
    FOR EACH ROW EXECUTE FUNCTION reject_resolved_exception_mutation();
