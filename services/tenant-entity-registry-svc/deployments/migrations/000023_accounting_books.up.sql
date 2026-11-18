-- 000023_accounting_books.up.sql
--
-- REF-06 Accounting Book / Ledger Basis (ZS-SVC-I-001 s4.16).
--
-- A book is the identity of a set of accounts: statutory, management, tax, consolidation or regulatory. Its basis (the
-- framework, the functional and presentation currencies, the chart reference) is versioned and effective-dated, so
-- what basis a book had on a date is always answerable and a basis change is prospective: it never rewrites how earlier
-- periods were booked. Its status moves Draft, Active, PostingSuspended, Retired, each move recorded as a transition so
-- "could this book post on that date" is an exact reconstruction. A book owns no ledger lines or balances (ACC-05) and no
-- accounting policy content (REF-07). Nothing is deleted.

CREATE TABLE accounting_books (
    book_id           UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(tenant_id),
    -- A book belongs to one legal entity, or to a group (consolidation and management books).
    legal_entity_id   UUID REFERENCES legal_entities(legal_entity_id),
    group_code        VARCHAR(40),
    book_code         VARCHAR(40) NOT NULL,
    name              TEXT NOT NULL,
    book_type         VARCHAR(16) NOT NULL,
    purpose           TEXT NOT NULL,
    status            VARCHAR(18) NOT NULL DEFAULT 'DRAFT',
    -- Statutory, tax and regulatory books are material: activated by someone other than their creator.
    material          BOOLEAN NOT NULL,
    current_basis     INTEGER NOT NULL DEFAULT 1,
    record_version    BIGINT NOT NULL DEFAULT 1,
    created_by        VARCHAR(255) NOT NULL,
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    activated_by      VARCHAR(255),
    correlation_id    VARCHAR(255),
    CONSTRAINT ab_tenant_unique UNIQUE (tenant_id, book_id),
    CONSTRAINT ab_one_scope CHECK ((legal_entity_id IS NULL) <> (group_code IS NULL)),
    CONSTRAINT ab_type_known CHECK (book_type IN ('STATUTORY', 'MANAGEMENT', 'TAX', 'CONSOLIDATION', 'REGULATORY')),
    CONSTRAINT ab_status_known CHECK (status IN ('DRAFT', 'ACTIVE', 'POSTING_SUSPENDED', 'RETIRED')),
    -- A statutory, tax or regulatory book is an entity's; only consolidation and management books belong to a group.
    CONSTRAINT ab_group_book_types CHECK (group_code IS NULL OR book_type IN ('CONSOLIDATION', 'MANAGEMENT')),
    CONSTRAINT ab_code_shape CHECK (book_code ~ '^[A-Z0-9][A-Z0-9_.-]{0,39}$'),
    CONSTRAINT ab_present CHECK (btrim(name) <> '' AND btrim(purpose) <> ''),
    CONSTRAINT ab_version_positive CHECK (record_version > 0 AND current_basis > 0),
    -- s4.16: a book's creator cannot self-approve the activation of a material statutory-basis book.
    CONSTRAINT ab_independent_activation CHECK (activated_by IS NULL OR NOT material OR activated_by <> created_by)
);
-- A book code is unique within its entity or group, whatever the book's status: a retired code is not reused.
CREATE UNIQUE INDEX ab_code_per_entity ON accounting_books (tenant_id, legal_entity_id, book_code) WHERE legal_entity_id IS NOT NULL;
CREATE UNIQUE INDEX ab_code_per_group ON accounting_books (tenant_id, group_code, book_code) WHERE group_code IS NOT NULL;
CREATE INDEX idx_ab_entity ON accounting_books (tenant_id, legal_entity_id, status);

CREATE OR REPLACE FUNCTION book_entity_tenant_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.legal_entity_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM legal_entities WHERE legal_entity_id = NEW.legal_entity_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'a book belongs only to a legal entity of its own tenant' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER book_entity_tenant_guard BEFORE INSERT OR UPDATE OF legal_entity_id, tenant_id ON accounting_books
    FOR EACH ROW EXECUTE FUNCTION book_entity_tenant_guard();

-- The basis a book had, by effective date. Content is immutable; a change is the next version.
CREATE TABLE book_basis_versions (
    basis_id              UUID PRIMARY KEY,
    tenant_id             UUID NOT NULL,
    book_id               UUID NOT NULL,
    version_no            INTEGER NOT NULL,
    framework             VARCHAR(20) NOT NULL,
    -- The named standard behind LOCAL_GAAP or a regulatory basis (for example "UK GAAP FRS 102").
    framework_detail      TEXT,
    functional_currency   CHAR(3) NOT NULL,
    presentation_currency CHAR(3) NOT NULL,
    chart_ref             TEXT,
    policy_ref            TEXT,
    valid_from            DATE NOT NULL,
    valid_to              DATE,
    reason                TEXT,
    evidence_ref          TEXT NOT NULL,
    recorded_by           VARCHAR(255) NOT NULL,
    recorded_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id        VARCHAR(255),
    CONSTRAINT bbv_book_in_tenant FOREIGN KEY (tenant_id, book_id) REFERENCES accounting_books (tenant_id, book_id),
    CONSTRAINT bbv_version_unique UNIQUE (book_id, version_no),
    CONSTRAINT bbv_framework_known CHECK (framework IN
        ('IFRS', 'IFRS_SME', 'US_GAAP', 'LOCAL_GAAP', 'TAX_BASIS', 'MANAGEMENT_BASIS', 'REGULATORY_BASIS')),
    CONSTRAINT bbv_currency_shape CHECK (functional_currency ~ '^[A-Z]{3}$' AND presentation_currency ~ '^[A-Z]{3}$'),
    -- A draft book's basis may be corrected on the day it starts: the replaced version is then zero-length and never in force.
    CONSTRAINT bbv_interval CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT bbv_evidence CHECK (btrim(evidence_ref) <> ''),
    CONSTRAINT bbv_local_named CHECK (framework NOT IN ('LOCAL_GAAP', 'REGULATORY_BASIS') OR btrim(COALESCE(framework_detail, '')) <> '')
);
CREATE UNIQUE INDEX bbv_one_open ON book_basis_versions (tenant_id, book_id) WHERE valid_to IS NULL;
CREATE INDEX idx_bbv_book ON book_basis_versions (tenant_id, book_id, valid_from);

-- What happened to a book's status, and when.
CREATE TABLE book_transitions (
    transition_id  UUID PRIMARY KEY,
    tenant_id      UUID NOT NULL,
    book_id        UUID NOT NULL,
    seq            INTEGER NOT NULL,
    from_status    VARCHAR(18),
    to_status      VARCHAR(18) NOT NULL,
    effective_date DATE NOT NULL,
    command        VARCHAR(24) NOT NULL,
    reason         TEXT,
    evidence_ref   TEXT NOT NULL,
    actor          VARCHAR(255) NOT NULL,
    version_before BIGINT,
    version_after  BIGINT NOT NULL,
    occurred_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id VARCHAR(255),
    CONSTRAINT bt_book_in_tenant FOREIGN KEY (tenant_id, book_id) REFERENCES accounting_books (tenant_id, book_id),
    CONSTRAINT bt_seq_unique UNIQUE (book_id, seq),
    CONSTRAINT bt_status_known CHECK (to_status IN ('DRAFT', 'ACTIVE', 'POSTING_SUSPENDED', 'RETIRED')
        AND (from_status IS NULL OR from_status IN ('DRAFT', 'ACTIVE', 'POSTING_SUSPENDED', 'RETIRED'))),
    CONSTRAINT bt_evidence CHECK (btrim(evidence_ref) <> '')
);
CREATE INDEX idx_bt_asof ON book_transitions (tenant_id, book_id, effective_date, seq);

CREATE OR REPLACE FUNCTION accounting_book_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounting books are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR NEW.group_code IS DISTINCT FROM OLD.group_code
       OR NEW.book_code <> OLD.book_code OR NEW.book_type <> OLD.book_type OR NEW.material <> OLD.material
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a book''s identity is immutable; open another book' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired book cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER accounting_book_guard BEFORE UPDATE OR DELETE ON accounting_books
    FOR EACH ROW EXECUTE FUNCTION accounting_book_guard();

CREATE OR REPLACE FUNCTION book_basis_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'book basis versions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.book_id <> OLD.book_id OR NEW.version_no <> OLD.version_no OR NEW.framework <> OLD.framework
       OR NEW.framework_detail IS DISTINCT FROM OLD.framework_detail OR NEW.functional_currency <> OLD.functional_currency
       OR NEW.presentation_currency <> OLD.presentation_currency OR NEW.chart_ref IS DISTINCT FROM OLD.chart_ref
       OR NEW.policy_ref IS DISTINCT FROM OLD.policy_ref OR NEW.valid_from <> OLD.valid_from
       OR NEW.evidence_ref <> OLD.evidence_ref OR NEW.recorded_by <> OLD.recorded_by OR NEW.recorded_at <> OLD.recorded_at THEN
        RAISE EXCEPTION 'a book basis version is immutable; amend the basis' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
        RAISE EXCEPTION 'a closed basis version cannot change its end' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER book_basis_guard BEFORE UPDATE OR DELETE ON book_basis_versions
    FOR EACH ROW EXECUTE FUNCTION book_basis_guard();

CREATE OR REPLACE FUNCTION book_transition_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'book transitions are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER book_transition_guard BEFORE UPDATE OR DELETE ON book_transitions
    FOR EACH ROW EXECUTE FUNCTION book_transition_guard();

ALTER TABLE accounting_books ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_books FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accounting_books FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE book_basis_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE book_basis_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON book_basis_versions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE book_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE book_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON book_transitions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
