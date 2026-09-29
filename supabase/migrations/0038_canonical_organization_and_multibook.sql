-- 0038_canonical_organization_and_multibook.sql
-- ZoikoSuite Canonical Organization, Bitemporal Corporate Hierarchy & Multi-Book Accounting
-- (ZS-DATA-001 §10, §11, Invariant D04 & D05)
--
-- DOCTRINE:
-- 1. Multi-Book Accounting (REF-BOOK / REF-LEDGER): LegalEntity → AccountingBook → Ledger.
--    Statutory, management, and tax books can diverge while sharing the same source documents.
-- 2. Physical establishment vs Legal Entity (ORG-EST): Tax and operational registrations attach
--    to establishments (branches, PEs, warehouses) as well as the incorporating entity.
-- 3. Bitemporal Corporate Relationships (ORG-REL): Ownership/control relationships are bitemporal
--    (valid-time + system-time) with exact decimal percentages (NUMERIC(38,18)).

CREATE SCHEMA IF NOT EXISTS organization_master;
CREATE SCHEMA IF NOT EXISTS accounting_reference;

GRANT USAGE ON SCHEMA organization_master TO zoiko_backend, authenticated;
GRANT USAGE ON SCHEMA accounting_reference TO zoiko_backend, authenticated;

-- ── establishments (ORG-EST) ──────────────────────────────────────────────────

CREATE TABLE organization_master.establishments (
    establishment_id                    UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    establishment_type                  VARCHAR(40)   NOT NULL, -- 'REGISTERED_OFFICE' | 'BRANCH' | 'WAREHOUSE' | 'PERMANENT_ESTABLISHMENT' | 'SITE'
    address_id                          UUID          NOT NULL,
    jurisdiction_id                     UUID          NOT NULL,
    tax_relevance                       VARCHAR(20)   NOT NULL DEFAULT 'NONE', -- 'NONE' | 'POTENTIAL' | 'REGISTERED' | 'PE'

    valid_from                          DATE          NOT NULL,
    valid_to                            DATE,
    recorded_at                         TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    superseded_at                       TIMESTAMPTZ,

    CONSTRAINT chk_est_type CHECK (establishment_type IN (
        'REGISTERED_OFFICE', 'BRANCH', 'WAREHOUSE', 'PERMANENT_ESTABLISHMENT', 'SITE'
    )),
    CONSTRAINT chk_est_tax_relevance CHECK (tax_relevance IN ('NONE', 'POTENTIAL', 'REGISTERED', 'PE')),
    CONSTRAINT chk_est_valid_dates CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT chk_est_superseded CHECK (superseded_at IS NULL OR superseded_at >= recorded_at),
    CONSTRAINT fk_est_address FOREIGN KEY (address_id) REFERENCES party_master.addresses(address_id) ON DELETE RESTRICT
);

CREATE INDEX idx_establishments_tenant_entity ON organization_master.establishments (tenant_id, legal_entity_id);
CREATE INDEX idx_establishments_jurisdiction ON organization_master.establishments (tenant_id, jurisdiction_id);

ALTER TABLE organization_master.establishments ENABLE ROW LEVEL SECURITY;
ALTER TABLE organization_master.establishments FORCE ROW LEVEL SECURITY;

CREATE POLICY establishments_tenant_isolation ON organization_master.establishments
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON organization_master.establishments TO zoiko_backend, authenticated;

-- ── corporate_relationships (ORG-REL) ─────────────────────────────────────────

CREATE TABLE organization_master.corporate_relationships (
    relationship_id                     UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    parent_entity_or_party_id           UUID          NOT NULL,
    child_legal_entity_id               UUID          NOT NULL,
    relationship_type                   VARCHAR(40)   NOT NULL, -- 'PARENT_SUBSIDIARY' | 'BRANCH' | 'JOINT_VENTURE' | 'ASSOCIATE' | 'BENEFICIAL_OWNERSHIP'
    ownership_percentage                NUMERIC(38,18) NOT NULL DEFAULT 0.000000000000000000,
    voting_percentage                   NUMERIC(38,18) NOT NULL DEFAULT 0.000000000000000000,
    consolidation_method                VARCHAR(20)   NOT NULL DEFAULT 'NONE', -- 'FULL' | 'EQUITY' | 'PROPORTIONATE' | 'NONE'
    evidence_ref                        UUID,

    valid_from                          DATE          NOT NULL,
    valid_to                            DATE,
    recorded_at                         TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    superseded_at                       TIMESTAMPTZ,

    CONSTRAINT chk_corp_rel_not_self CHECK (parent_entity_or_party_id <> child_legal_entity_id),
    CONSTRAINT chk_corp_rel_type CHECK (relationship_type IN (
        'PARENT_SUBSIDIARY', 'BRANCH', 'JOINT_VENTURE', 'ASSOCIATE', 'BENEFICIAL_OWNERSHIP'
    )),
    CONSTRAINT chk_corp_consolidation CHECK (consolidation_method IN ('FULL', 'EQUITY', 'PROPORTIONATE', 'NONE')),
    CONSTRAINT chk_corp_rel_dates CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT chk_corp_rel_superseded CHECK (superseded_at IS NULL OR superseded_at >= recorded_at)
);

CREATE INDEX idx_corp_rel_tenant_parent ON organization_master.corporate_relationships (tenant_id, parent_entity_or_party_id);
CREATE INDEX idx_corp_rel_tenant_child ON organization_master.corporate_relationships (tenant_id, child_legal_entity_id);

ALTER TABLE organization_master.corporate_relationships ENABLE ROW LEVEL SECURITY;
ALTER TABLE organization_master.corporate_relationships FORCE ROW LEVEL SECURITY;

CREATE POLICY corporate_relationships_tenant_isolation ON organization_master.corporate_relationships
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON organization_master.corporate_relationships TO zoiko_backend, authenticated;

-- ── accounting_books (REF-BOOK) ──────────────────────────────────────────────

CREATE TABLE accounting_reference.accounting_books (
    accounting_book_id                  UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    legal_entity_id                     UUID          NOT NULL,
    book_code                           VARCHAR(50)   NOT NULL,
    book_type                           VARCHAR(30)   NOT NULL, -- 'STATUTORY' | 'MANAGEMENT' | 'TAX' | 'CONSOLIDATION' | 'OTHER'
    accounting_framework                VARCHAR(50)   NOT NULL, -- 'IFRS' | 'US_GAAP' | 'UK_GAAP' | 'LOCAL_STATUTORY'
    functional_currency                 VARCHAR(3)    NOT NULL,
    reporting_currency                  VARCHAR(3),
    fiscal_calendar_id                  UUID          NOT NULL,

    valid_from                          DATE          NOT NULL,
    valid_to                            DATE,
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_book_type CHECK (book_type IN ('STATUTORY', 'MANAGEMENT', 'TAX', 'CONSOLIDATION', 'OTHER')),
    CONSTRAINT chk_book_dates CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT uq_entity_book_code UNIQUE (tenant_id, legal_entity_id, book_code)
);

CREATE INDEX idx_accounting_books_tenant_entity ON accounting_reference.accounting_books (tenant_id, legal_entity_id);

ALTER TABLE accounting_reference.accounting_books ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_reference.accounting_books FORCE ROW LEVEL SECURITY;

CREATE POLICY accounting_books_tenant_isolation ON accounting_reference.accounting_books
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON accounting_reference.accounting_books TO zoiko_backend, authenticated;

-- ── ledgers (REF-LEDGER) ─────────────────────────────────────────────────────

CREATE TABLE accounting_reference.ledgers (
    ledger_id                           UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    accounting_book_id                  UUID          NOT NULL,
    ledger_code                         VARCHAR(50)   NOT NULL,
    ledger_type                         VARCHAR(30)   NOT NULL, -- 'PRIMARY' | 'ADJUSTMENT' | 'TAX' | 'ELIMINATION' | 'STATISTICAL'
    posting_currency_basis              VARCHAR(20)   NOT NULL DEFAULT 'FUNCTIONAL', -- 'FUNCTIONAL' | 'TRANSACTION' | 'REPORTING'
    is_active                           BOOLEAN       NOT NULL DEFAULT TRUE,

    valid_from                          DATE          NOT NULL,
    valid_to                            DATE,
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_ledger_type CHECK (ledger_type IN ('PRIMARY', 'ADJUSTMENT', 'TAX', 'ELIMINATION', 'STATISTICAL')),
    CONSTRAINT chk_ledger_currency_basis CHECK (posting_currency_basis IN ('FUNCTIONAL', 'TRANSACTION', 'REPORTING')),
    CONSTRAINT chk_ledger_dates CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT uq_book_ledger_code UNIQUE (tenant_id, accounting_book_id, ledger_code),
    CONSTRAINT fk_ledgers_accounting_book FOREIGN KEY (accounting_book_id) REFERENCES accounting_reference.accounting_books(accounting_book_id) ON DELETE RESTRICT
);

CREATE INDEX idx_ledgers_tenant_book ON accounting_reference.ledgers (tenant_id, accounting_book_id);

ALTER TABLE accounting_reference.ledgers ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_reference.ledgers FORCE ROW LEVEL SECURITY;

CREATE POLICY ledgers_tenant_isolation ON accounting_reference.ledgers
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON accounting_reference.ledgers TO zoiko_backend, authenticated;
