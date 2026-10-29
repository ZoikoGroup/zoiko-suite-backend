-- Global Accounting Kernel (standard §28): opening balances, migration and cutover. Migration is not a special permission to bypass
-- financial integrity: a batch is staged, validated, previewed, approved by a named authority inside a cutover window, posted as one
-- OPENING journal, reconciled to the source and then frozen with its evidence.

CREATE TABLE migration_batches (
    batch_id             UUID PRIMARY KEY,
    tenant_id            UUID          NOT NULL,
    legal_entity_id      UUID          NOT NULL,
    accounting_book_id   UUID          NOT NULL REFERENCES accounting_books(book_id),
    batch_code           VARCHAR(64)   NOT NULL,
    batch_kind           VARCHAR(12)   NOT NULL,
    corrects_batch_id    UUID          REFERENCES migration_batches(batch_id),
    cutover_date         DATE          NOT NULL,
    currency             CHAR(3)       NOT NULL,
    source_system        VARCHAR(120)  NOT NULL,
    source_extract_ref   VARCHAR(255)  NOT NULL,
    source_extract_hash  VARCHAR(64)   NOT NULL,
    mapping_ref          VARCHAR(255)  NOT NULL,
    source_totals        JSONB         NOT NULL,
    control_totals       JSONB         NOT NULL DEFAULT '{}'::jsonb,
    line_count           INTEGER       NOT NULL,
    total_debit          NUMERIC(24,2) NOT NULL,
    total_credit         NUMERIC(24,2) NOT NULL,
    lines_hash           VARCHAR(64)   NOT NULL,
    status               VARCHAR(12)   NOT NULL,
    validation_report    JSONB,
    created_by           VARCHAR(255)  NOT NULL,
    created_at           TIMESTAMPTZ   NOT NULL,
    validated_by         VARCHAR(255),
    validated_at         TIMESTAMPTZ,
    approved_by          VARCHAR(255),
    approved_at          TIMESTAMPTZ,
    authority_ref        VARCHAR(255),
    window_start         TIMESTAMPTZ,
    window_end           TIMESTAMPTZ,
    journal_id           UUID,
    posted_by            VARCHAR(255),
    posted_at            TIMESTAMPTZ,
    reconciliation       JSONB,
    reconciled_by        VARCHAR(255),
    reconciled_at        TIMESTAMPTZ,
    evidence_hash        VARCHAR(64),
    cancelled_by         VARCHAR(255),
    cancelled_at         TIMESTAMPTZ,
    cancel_reason        TEXT,
    CONSTRAINT migration_batches_status CHECK (status IN ('STAGED', 'VALIDATED', 'APPROVED', 'POSTED', 'RECONCILED', 'CANCELLED')),
    CONSTRAINT migration_batches_kind CHECK (batch_kind IN ('OPENING', 'CORRECTION')),
    CONSTRAINT migration_batches_corrects CHECK ((batch_kind = 'CORRECTION') = (corrects_batch_id IS NOT NULL)),
    CONSTRAINT migration_batches_text CHECK (btrim(batch_code) <> '' AND btrim(source_system) <> '' AND btrim(source_extract_ref) <> '' AND btrim(mapping_ref) <> ''),
    CONSTRAINT migration_batches_hash CHECK (source_extract_hash ~ '^[0-9a-f]{64}$' AND lines_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT migration_batches_approval CHECK (status IN ('STAGED', 'VALIDATED', 'CANCELLED') OR (approved_by IS NOT NULL AND authority_ref IS NOT NULL AND window_start IS NOT NULL AND window_end > window_start)),
    CONSTRAINT migration_batches_posted CHECK (status NOT IN ('POSTED', 'RECONCILED') OR (journal_id IS NOT NULL AND posted_by IS NOT NULL)),
    CONSTRAINT migration_batches_reconciled CHECK (status <> 'RECONCILED' OR (evidence_hash IS NOT NULL AND reconciliation->>'status' = 'MATCHED')),
    CONSTRAINT migration_batches_cancelled CHECK (status <> 'CANCELLED' OR (cancelled_by IS NOT NULL AND COALESCE(btrim(cancel_reason), '') <> '')),
    CONSTRAINT migration_batches_author_approver CHECK (approved_by IS NULL OR approved_by <> created_by)
);
CREATE UNIQUE INDEX uq_migration_batches_code ON migration_batches (tenant_id, legal_entity_id, accounting_book_id, batch_code);
-- one live opening per book: a later correction is its own batch that names the one it corrects
CREATE UNIQUE INDEX uq_migration_batches_live_opening ON migration_batches (tenant_id, legal_entity_id, accounting_book_id)
    WHERE batch_kind = 'OPENING' AND status IN ('POSTED', 'RECONCILED');
CREATE INDEX idx_migration_batches_entity ON migration_batches (tenant_id, legal_entity_id, status);
ALTER TABLE migration_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON migration_batches
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- What a batch is, the extract it came from and the figures it is held to never change; only the lifecycle moves, forward.
CREATE OR REPLACE FUNCTION migration_batches_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a migration batch is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.batch_id, NEW.tenant_id, NEW.legal_entity_id, NEW.accounting_book_id, NEW.batch_code, NEW.batch_kind, NEW.corrects_batch_id, NEW.cutover_date, NEW.currency,
        NEW.source_system, NEW.source_extract_ref, NEW.source_extract_hash, NEW.mapping_ref, NEW.source_totals::text, NEW.control_totals::text, NEW.line_count,
        NEW.total_debit, NEW.total_credit, NEW.lines_hash, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.batch_id, OLD.tenant_id, OLD.legal_entity_id, OLD.accounting_book_id, OLD.batch_code, OLD.batch_kind, OLD.corrects_batch_id, OLD.cutover_date, OLD.currency,
        OLD.source_system, OLD.source_extract_ref, OLD.source_extract_hash, OLD.mapping_ref, OLD.source_totals::text, OLD.control_totals::text, OLD.line_count,
        OLD.total_debit, OLD.total_credit, OLD.lines_hash, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'what a migration batch extracted, mapped and is held to is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('RECONCILED', 'CANCELLED') THEN
        RAISE EXCEPTION 'a % migration batch is final', lower(OLD.status) USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
           (OLD.status = 'STAGED' AND NEW.status IN ('VALIDATED', 'CANCELLED'))
        OR (OLD.status = 'VALIDATED' AND NEW.status IN ('APPROVED', 'CANCELLED'))
        OR (OLD.status = 'APPROVED' AND NEW.status IN ('POSTED', 'CANCELLED'))
        OR (OLD.status = 'POSTED' AND NEW.status = 'RECONCILED')) THEN
        RAISE EXCEPTION 'a migration batch cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.approved_by IS NOT NULL AND NEW.approved_by = NEW.created_by THEN
        RAISE EXCEPTION 'a migration batch must be approved by someone other than the person who staged it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.approved_by IS NOT NULL AND (NEW.approved_by, NEW.authority_ref, NEW.window_start, NEW.window_end) IS DISTINCT FROM (OLD.approved_by, OLD.authority_ref, OLD.window_start, OLD.window_end) THEN
        RAISE EXCEPTION 'the approval of a migration batch is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.journal_id IS NOT NULL AND NEW.journal_id IS DISTINCT FROM OLD.journal_id THEN
        RAISE EXCEPTION 'the journal a migration batch posted is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER migration_batches_guard_trg BEFORE UPDATE OR DELETE ON migration_batches FOR EACH ROW EXECUTE FUNCTION migration_batches_guard();

CREATE TABLE migration_batch_lines (
    tenant_id            UUID          NOT NULL,
    batch_id             UUID          NOT NULL REFERENCES migration_batches(batch_id),
    line_no              INTEGER       NOT NULL,
    source_ref           VARCHAR(120)  NOT NULL,
    source_account       VARCHAR(64)   NOT NULL,
    account_code         VARCHAR(64)   NOT NULL,
    debit_amount         NUMERIC(24,2) NOT NULL,
    credit_amount        NUMERIC(24,2) NOT NULL,
    counterparty_id      VARCHAR(200),
    transaction_amount   NUMERIC(28,8),
    transaction_currency CHAR(3),
    exchange_rate        NUMERIC(28,12),
    dimensions           JSONB,
    description          TEXT          NOT NULL DEFAULT '',
    PRIMARY KEY (batch_id, line_no),
    CONSTRAINT migration_batch_lines_side CHECK ((debit_amount > 0 AND credit_amount = 0) OR (credit_amount > 0 AND debit_amount = 0)),
    CONSTRAINT migration_batch_lines_tx CHECK ((transaction_amount IS NULL AND transaction_currency IS NULL AND exchange_rate IS NULL)
        OR (transaction_amount IS NOT NULL AND transaction_currency IS NOT NULL AND exchange_rate IS NOT NULL AND exchange_rate > 0)),
    CONSTRAINT migration_batch_lines_ref UNIQUE (batch_id, source_ref)
);
ALTER TABLE migration_batch_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_batch_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON migration_batch_lines
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE TABLE migration_batch_events (
    event_id    UUID PRIMARY KEY,
    tenant_id   UUID          NOT NULL,
    batch_id    UUID          NOT NULL REFERENCES migration_batches(batch_id),
    kind        VARCHAR(24)   NOT NULL,
    actor       VARCHAR(255)  NOT NULL,
    at          TIMESTAMPTZ   NOT NULL,
    detail      JSONB         NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX idx_migration_batch_events_batch ON migration_batch_events (tenant_id, batch_id, at);
ALTER TABLE migration_batch_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_batch_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON migration_batch_events
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE OR REPLACE FUNCTION migration_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER migration_batch_lines_append_only BEFORE UPDATE OR DELETE ON migration_batch_lines FOR EACH ROW EXECUTE FUNCTION migration_append_only();
CREATE TRIGGER migration_batch_events_append_only BEFORE UPDATE OR DELETE ON migration_batch_events FOR EACH ROW EXECUTE FUNCTION migration_append_only();
