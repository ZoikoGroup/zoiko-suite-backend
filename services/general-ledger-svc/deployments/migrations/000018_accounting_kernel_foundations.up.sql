-- Migration: 000018_accounting_kernel_foundations.up.sql
--
-- Global Accounting Kernel, Wave 0 (kernel foundations): books, the immutable accounting event store, the versioned
-- posting-rule registry with its governance, and the financial-dimension registry.
--
-- What the database guarantees on its own:
--   * tenant isolation (RLS, FORCEd) on every table;
--   * a book keeps its identity, type, framework and functional currency; its status moves only along
--     DRAFT -> ACTIVE <-> SUSPENDED -> RETIRED and RETIRED is final;
--   * an accounting event is never changed or deleted (AK-INV-003/009), the same producer event at the same version
--     is stored once, and the same idempotency key is stored once;
--   * a posting rule set keeps its content from the moment it leaves DRAFT; it can be approved only for the exact
--     content and test pack that passed (tests_passed_hash = content_hash) by someone other than its author, and
--     activated only by someone other than its author; two ACTIVE rule sets never claim the same event type, book type
--     and framework on a common date (checked under an advisory lock, so concurrent activations cannot both win);
--   * dimension values are never renamed or deleted: they are retired.
--
-- Existing journals and ledger entries carry a free-text book_id. This migration registers one book per
-- (tenant, legal entity, book_id) found in journal_headers, so every journal's book resolves to a registered book.
-- Their framework and calendar are 'LEGACY'/'DEFAULT' and their functional currency is the currency most journals of
-- that entity and book were posted in; a book whose journals carry no currency is created as DRAFT for the owner to
-- complete.

CREATE TABLE accounting_books (
    book_id                  UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    legal_entity_id          UUID NOT NULL,
    book_code                VARCHAR(63) NOT NULL,
    book_type                VARCHAR(24) NOT NULL,
    accounting_framework_id  VARCHAR(63) NOT NULL,
    functional_currency      CHAR(3) NOT NULL,
    fiscal_calendar_id       VARCHAR(63) NOT NULL,
    chart_of_accounts_id     VARCHAR(63) NOT NULL,
    default_ledger_id        VARCHAR(63) NOT NULL,
    effective_from           DATE NOT NULL,
    effective_to             DATE,
    status                   VARCHAR(10) NOT NULL DEFAULT 'DRAFT',
    created_by               VARCHAR(255) NOT NULL,
    version                  BIGINT NOT NULL DEFAULT 1,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT accounting_books_code UNIQUE (tenant_id, legal_entity_id, book_code),
    CONSTRAINT accounting_books_type CHECK (book_type IN ('PRIMARY_STATUTORY', 'MANAGEMENT', 'TAX', 'GROUP_CONSOLIDATION', 'REGULATORY')),
    CONSTRAINT accounting_books_status CHECK (status IN ('DRAFT', 'ACTIVE', 'SUSPENDED', 'RETIRED')),
    CONSTRAINT accounting_books_period CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT accounting_books_ccy CHECK (functional_currency ~ '^[A-Z]{3}$')
);
CREATE INDEX accounting_books_entity ON accounting_books (tenant_id, legal_entity_id, status);

CREATE OR REPLACE FUNCTION accounting_books_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an accounting book is never deleted: retire it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.book_id <> OLD.book_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.book_code <> OLD.book_code
       OR NEW.book_type <> OLD.book_type OR NEW.accounting_framework_id <> OLD.accounting_framework_id
       OR NEW.functional_currency <> OLD.functional_currency OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity, type, framework and functional currency of a book are fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'book version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired book is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
           (OLD.status = 'DRAFT' AND NEW.status IN ('ACTIVE', 'RETIRED'))
        OR (OLD.status = 'ACTIVE' AND NEW.status IN ('SUSPENDED', 'RETIRED'))
        OR (OLD.status = 'SUSPENDED' AND NEW.status IN ('ACTIVE', 'RETIRED'))) THEN
        RAISE EXCEPTION 'book status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER accounting_books_guard_trg BEFORE UPDATE OR DELETE ON accounting_books FOR EACH ROW EXECUTE FUNCTION accounting_books_guard();

-- one book per (tenant, legal entity, journal book_id) already in use
INSERT INTO accounting_books (book_id, tenant_id, legal_entity_id, book_code, book_type, accounting_framework_id, functional_currency, fiscal_calendar_id,
                              chart_of_accounts_id, default_ledger_id, effective_from, status, created_by)
SELECT gen_random_uuid(), tenant_id, legal_entity_id, book_code, 'PRIMARY_STATUTORY', 'LEGACY', COALESCE(ccy, 'USD'), 'DEFAULT', 'DEFAULT', 'DEFAULT',
       DATE '1970-01-01', CASE WHEN ccy IS NULL THEN 'DRAFT' ELSE 'ACTIVE' END, 'migration:000018'
FROM (
    SELECT tenant_id, legal_entity_id, COALESCE(NULLIF(book_id, ''), 'DEFAULT') AS book_code,
           mode() WITHIN GROUP (ORDER BY currency_code) FILTER (WHERE currency_code ~ '^[A-Z]{3}$') AS ccy
    FROM journal_headers
    GROUP BY tenant_id, legal_entity_id, COALESCE(NULLIF(book_id, ''), 'DEFAULT')
) j
WHERE book_code ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$';

CREATE TABLE accounting_events (
    accounting_event_id     UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    legal_entity_id         UUID NOT NULL,
    event_type              VARCHAR(80) NOT NULL,
    event_version           INTEGER NOT NULL,
    source_system           VARCHAR(200) NOT NULL,
    source_object_type      VARCHAR(200) NOT NULL,
    source_object_id        VARCHAR(200) NOT NULL,
    source_event_id         VARCHAR(200) NOT NULL,
    occurred_at             TIMESTAMPTZ NOT NULL,
    effective_at            TIMESTAMPTZ,
    document_date           DATE,
    posting_date_requested  DATE,
    transaction_currency    CHAR(3),
    facts                   JSONB NOT NULL,
    counterparty_id         VARCHAR(200),
    dimensions              JSONB,
    evidence_refs           JSONB,
    idempotency_key         VARCHAR(200) NOT NULL,
    correlation_id          VARCHAR(200) NOT NULL,
    causation_id            VARCHAR(200) NOT NULL,
    payload_hash            CHAR(64) NOT NULL,
    registered_by           VARCHAR(255) NOT NULL,
    registered_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT accounting_events_source UNIQUE (tenant_id, source_system, source_event_id, event_version),
    CONSTRAINT accounting_events_idem UNIQUE (tenant_id, idempotency_key),
    CONSTRAINT accounting_events_version CHECK (event_version >= 1)
);
CREATE INDEX accounting_events_type ON accounting_events (tenant_id, legal_entity_id, event_type, occurred_at);
CREATE INDEX accounting_events_source_obj ON accounting_events (tenant_id, source_object_type, source_object_id);

CREATE OR REPLACE FUNCTION accounting_events_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'an accounting event is never changed or deleted: a correction is a new event that names the one it corrects' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER accounting_events_immutable_trg BEFORE UPDATE OR DELETE ON accounting_events FOR EACH ROW EXECUTE FUNCTION accounting_events_immutable();

CREATE TABLE posting_rule_sets (
    posting_rule_set_id      UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    name                     VARCHAR(63) NOT NULL,
    domain                   VARCHAR(63) NOT NULL,
    event_type               VARCHAR(80) NOT NULL,
    book_type                VARCHAR(24) NOT NULL,
    accounting_framework_id  VARCHAR(63) NOT NULL,
    version                  VARCHAR(20) NOT NULL,
    status                   VARCHAR(10) NOT NULL DEFAULT 'DRAFT',
    effective_from           DATE NOT NULL,
    effective_to             DATE,
    content                  JSONB NOT NULL,
    content_hash             CHAR(64) NOT NULL,
    test_pack                JSONB NOT NULL,
    test_pack_id             VARCHAR(63) NOT NULL,
    tests_passed_hash        CHAR(64),
    source_authority_refs    JSONB,
    created_by               VARCHAR(255) NOT NULL,
    approved_by              VARCHAR(255),
    approved_at              TIMESTAMPTZ,
    activated_by             VARCHAR(255),
    activated_at             TIMESTAMPTZ,
    row_version              BIGINT NOT NULL DEFAULT 1,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT posting_rule_sets_version UNIQUE (tenant_id, name, version),
    CONSTRAINT posting_rule_sets_status CHECK (status IN ('DRAFT', 'TEST', 'APPROVED', 'ACTIVE', 'SUPERSEDED', 'RETIRED')),
    CONSTRAINT posting_rule_sets_book_type CHECK (book_type IN ('PRIMARY_STATUTORY', 'MANAGEMENT', 'TAX', 'GROUP_CONSOLIDATION', 'REGULATORY')),
    CONSTRAINT posting_rule_sets_period CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT posting_rule_sets_author_approver CHECK (approved_by IS NULL OR approved_by <> created_by),
    CONSTRAINT posting_rule_sets_author_activator CHECK (activated_by IS NULL OR activated_by <> created_by)
);
CREATE INDEX posting_rule_sets_scope ON posting_rule_sets (tenant_id, event_type, book_type, accounting_framework_id, status);

CREATE OR REPLACE FUNCTION posting_rule_sets_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a posting rule set is never deleted: retire it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'a posting rule set is created as a draft' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.posting_rule_set_id <> OLD.posting_rule_set_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.name <> OLD.name OR NEW.domain <> OLD.domain
       OR NEW.event_type <> OLD.event_type OR NEW.book_type <> OLD.book_type OR NEW.accounting_framework_id <> OLD.accounting_framework_id
       OR NEW.version <> OLD.version OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity of a posting rule set version is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'rule set version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'DRAFT' AND (NEW.content::text <> OLD.content::text OR NEW.content_hash <> OLD.content_hash OR NEW.test_pack::text <> OLD.test_pack::text
       OR NEW.test_pack_id <> OLD.test_pack_id OR NEW.effective_from <> OLD.effective_from) THEN
        RAISE EXCEPTION 'the content, test pack and start date of a rule set are fixed once it leaves draft' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired rule set is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.effective_to IS DISTINCT FROM NEW.effective_to AND OLD.status NOT IN ('DRAFT', 'ACTIVE') THEN
        RAISE EXCEPTION 'the end date of a rule set is fixed once it is superseded or retired' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status THEN
        IF NOT (
               (OLD.status = 'DRAFT' AND NEW.status = 'TEST')
            OR (OLD.status = 'TEST' AND NEW.status IN ('DRAFT', 'APPROVED'))
            OR (OLD.status = 'APPROVED' AND NEW.status IN ('ACTIVE', 'DRAFT'))
            OR (OLD.status = 'ACTIVE' AND NEW.status IN ('SUPERSEDED', 'RETIRED'))
            OR (OLD.status = 'SUPERSEDED' AND NEW.status = 'RETIRED')) THEN
            RAISE EXCEPTION 'rule set status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.status IN ('APPROVED', 'ACTIVE') AND (NEW.tests_passed_hash IS NULL OR NEW.tests_passed_hash <> NEW.content_hash) THEN
            RAISE EXCEPTION 'the test pack has not passed for exactly this content' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.status = 'APPROVED' AND (NEW.approved_by IS NULL OR NEW.approved_by = NEW.created_by) THEN
            RAISE EXCEPTION 'a rule set is approved by someone other than its author' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.status = 'ACTIVE' THEN
            IF NEW.activated_by IS NULL OR NEW.activated_by = NEW.created_by OR NEW.approved_by IS NULL THEN
                RAISE EXCEPTION 'a rule set is activated by someone other than its author, after approval' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id::text || '|' || NEW.event_type || '|' || NEW.book_type || '|' || NEW.accounting_framework_id));
            IF EXISTS (SELECT 1 FROM posting_rule_sets o
                       WHERE o.tenant_id = NEW.tenant_id AND o.event_type = NEW.event_type AND o.book_type = NEW.book_type
                         AND o.accounting_framework_id = NEW.accounting_framework_id AND o.status = 'ACTIVE' AND o.posting_rule_set_id <> NEW.posting_rule_set_id
                         AND o.effective_from < COALESCE(NEW.effective_to, DATE '9999-01-01')
                         AND NEW.effective_from < COALESCE(o.effective_to, DATE '9999-01-01')) THEN
                RAISE EXCEPTION 'an active rule set already covers this event type, book type and framework on a common date' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER posting_rule_sets_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON posting_rule_sets FOR EACH ROW EXECUTE FUNCTION posting_rule_sets_guard();

CREATE TABLE posting_rule_set_events (
    event_id            UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    posting_rule_set_id UUID NOT NULL REFERENCES posting_rule_sets(posting_rule_set_id),
    kind                VARCHAR(32) NOT NULL,
    actor_id            VARCHAR(255) NOT NULL,
    detail              JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX posting_rule_set_events_rule ON posting_rule_set_events (posting_rule_set_id, created_at);
CREATE OR REPLACE FUNCTION kernel_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'this record is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER posting_rule_set_events_no_change BEFORE UPDATE OR DELETE ON posting_rule_set_events FOR EACH ROW EXECUTE FUNCTION kernel_append_only();

CREATE TABLE dimension_definitions (
    tenant_id       UUID NOT NULL,
    dimension_code  VARCHAR(63) NOT NULL,
    name            VARCHAR(120) NOT NULL,
    status          VARCHAR(8) NOT NULL DEFAULT 'ACTIVE',
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, dimension_code),
    CONSTRAINT dimension_definitions_status CHECK (status IN ('ACTIVE', 'RETIRED'))
);

CREATE TABLE dimension_values (
    value_id         UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    dimension_code   VARCHAR(63) NOT NULL,
    value_code       VARCHAR(120) NOT NULL,
    name             VARCHAR(200) NOT NULL,
    legal_entity_id  UUID,
    parent_value_code VARCHAR(120),
    valid_from       DATE NOT NULL,
    valid_to         DATE,
    status           VARCHAR(8) NOT NULL DEFAULT 'ACTIVE',
    created_by       VARCHAR(255) NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, dimension_code) REFERENCES dimension_definitions (tenant_id, dimension_code),
    CONSTRAINT dimension_values_status CHECK (status IN ('ACTIVE', 'RETIRED')),
    CONSTRAINT dimension_values_period CHECK (valid_to IS NULL OR valid_to > valid_from)
);
CREATE UNIQUE INDEX dimension_values_unique ON dimension_values (tenant_id, dimension_code, value_code, COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::uuid));

CREATE OR REPLACE FUNCTION dimension_values_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a dimension value is never deleted: retire it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.value_id <> OLD.value_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.dimension_code <> OLD.dimension_code OR NEW.value_code <> OLD.value_code
       OR NEW.name <> OLD.name OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR NEW.parent_value_code IS DISTINCT FROM OLD.parent_value_code
       OR NEW.valid_from <> OLD.valid_from OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'a dimension value is never renamed or moved: retire it and create the new one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired dimension value is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER dimension_values_guard_trg BEFORE UPDATE OR DELETE ON dimension_values FOR EACH ROW EXECUTE FUNCTION dimension_values_guard();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['accounting_books', 'accounting_events', 'posting_rule_sets', 'posting_rule_set_events', 'dimension_definitions', 'dimension_values']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
