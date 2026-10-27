-- Migration: 000001_initial_schema.up.sql
--
-- financial-statements-svc (REP-01, ZS-SVC-H-001 §4.1): FinancialStatementSet
-- is the aggregate root, StatementFact its line items (one per source
-- account; see internal/domain's package doc comment for the aggregation and
-- cash-flow scope cuts this version makes).

CREATE TABLE financial_statement_sets (
    statement_set_id       UUID PRIMARY KEY,
    tenant_id               TEXT NOT NULL,
    legal_entity_id         TEXT NOT NULL,
    reporting_period        TEXT NOT NULL,
    framework                TEXT NOT NULL,
    presentation_currency   TEXT NOT NULL,
    source_snapshot_id      TEXT NOT NULL,
    source_snapshot_hash    TEXT NOT NULL,
    mapping_version         TEXT NOT NULL,
    status                  TEXT NOT NULL,
    version                 BIGINT NOT NULL DEFAULT 1,
    validation_errors       JSONB,

    supersedes_set_id       UUID REFERENCES financial_statement_sets(statement_set_id),
    superseded_by_set_id    UUID REFERENCES financial_statement_sets(statement_set_id),

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_principal_id    TEXT NOT NULL,

    last_refreshed_at                  TIMESTAMPTZ,
    last_refreshed_by_principal_id     TEXT,

    validated_at TIMESTAMPTZ,

    submitted_for_review_at                TIMESTAMPTZ,
    submitted_for_review_by_principal_id   TEXT,

    approved_at                 TIMESTAMPTZ,
    approved_by_principal_id    TEXT,

    certified_at                TIMESTAMPTZ,
    certified_by_principal_id   TEXT,
    certification_reason        TEXT,

    published_at    TIMESTAMPTZ,
    superseded_at   TIMESTAMPTZ,

    CONSTRAINT financial_statement_sets_status_valid CHECK (status IN
        ('DRAFT', 'VALIDATED', 'IN_REVIEW', 'APPROVED', 'CERTIFIED', 'PUBLISHED', 'SUPERSEDED')),
    CONSTRAINT financial_statement_sets_version_positive CHECK (version >= 1)
);

-- At most one non-superseded statement set per (entity, period, framework):
-- a correction is always a new set via SupersedeStatementSet, never two
-- independently-progressing drafts for the same real-world report.
CREATE UNIQUE INDEX financial_statement_sets_one_active_idx
    ON financial_statement_sets (tenant_id, legal_entity_id, reporting_period, framework)
    WHERE status <> 'SUPERSEDED';

CREATE INDEX financial_statement_sets_tenant_idx ON financial_statement_sets (tenant_id);

ALTER TABLE financial_statement_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_statement_sets FORCE ROW LEVEL SECURITY;

-- NULLIF guards the stale-GUC bug tenant-entity-registry-svc's migration
-- 000006 found: after Postgres resets a transaction-local 'SET LOCAL' to ''
-- on commit, a later connection-reusing session that never sets the GUC
-- again sees current_setting return '' (not NULL), and '' ::UUID/''::TEXT
-- comparisons behave unexpectedly rather than simply denying. NULLIF makes an
-- unset tenant compare as NULL, which no tenant_id ever equals.
CREATE POLICY financial_statement_sets_tenant_isolation ON financial_statement_sets
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE TABLE statement_facts (
    fact_id             UUID PRIMARY KEY,
    tenant_id            TEXT NOT NULL,
    statement_set_id    UUID NOT NULL REFERENCES financial_statement_sets(statement_set_id) ON DELETE CASCADE,
    statement_type       TEXT NOT NULL,
    category             TEXT NOT NULL,
    account_code         TEXT NOT NULL,
    account_name         TEXT NOT NULL,
    amount               NUMERIC(38,12) NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT statement_facts_statement_type_valid CHECK (statement_type IN ('BALANCE_SHEET', 'INCOME_STATEMENT')),
    CONSTRAINT statement_facts_category_valid CHECK (category IN ('ASSET', 'LIABILITY', 'EQUITY', 'REVENUE', 'EXPENSE')),
    CONSTRAINT statement_facts_unique_account UNIQUE (statement_set_id, account_code)
);

CREATE INDEX statement_facts_set_idx ON statement_facts (statement_set_id);

ALTER TABLE statement_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE statement_facts FORCE ROW LEVEL SECURITY;

CREATE POLICY statement_facts_tenant_isolation ON statement_facts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
