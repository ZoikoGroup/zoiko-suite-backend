-- 0018_obligations_svc.sql
-- obligations-svc → schema `obligations`
--
-- Squashed end state of 000001_initial_schema,
-- 000002_add_applicability_decisions and 000003_tenant_scoping_and_invariants.
-- Three tables: obligations, filing_requirements, applicability_decisions.
--
-- ── tenant_id is a column from the start here ────────────────────────────────
-- 000003 had to ADD it: this service had no tenant column at all — not a
-- missing filter, a missing dimension. The sharpest edge was the dedup key.
-- obligation_code carried a GLOBAL unique index and creation is idempotent on
-- it, so a second tenant registering an ordinary code like "VAT-Q1-2026" did
-- not create their obligation — it silently returned the FIRST tenant's, with
-- that tenant's legal entity, due date and source reference, as a 200. One
-- tenant's compliance register answering with another's record, through the
-- documented happy path.
--
-- The nullable-add / backfill / SET NOT NULL dance and its hardcoded demo
-- tenant do not travel here; the column is simply NOT NULL. That backfill was
-- flagged in 000003 as unsafe for a deployment with real multi-tenant history,
-- and this migration removes the question.
--
-- ── applicability_decisions GETS a tenant column ─────────────────────────────
-- This is the one open gap in known-gaps.md that this migration closes rather
-- than carries. The table arrived from origin/main independently of the
-- tenant-scoping pass, so it has no tenant_id, is not covered by the row-level
-- security 000003 installed, and its store names no tenant in any of its three
-- statements. It is reachable only through an obligation_id, which IS
-- tenant-scoped, but the queries never join back — so an obligation_id
-- belonging to another tenant returns that tenant's applicability decisions,
-- including facts_used and who decided.
--
-- The fix here is the one the note prescribes: a tenant_id column, a composite
-- foreign key that keeps it agreeing with the parent obligation, and FORCE RLS
-- with a WITH CHECK policy.
--
-- ── Still open after this migration ──────────────────────────────────────────
-- Nothing verifies that an obligation's legal_entity_id belongs to the caller's
-- tenant. Authorization is scoped to the entity and rows are scoped by tenant,
-- and the two are never reconciled — a database-side fix is not available,
-- because no service on the platform calls tenant-entity-registry-svc at all.

CREATE SCHEMA IF NOT EXISTS obligations;

COMMENT ON SCHEMA obligations IS
    'obligations-svc. Statutory, regulatory, contractual and internal-policy obligations, their filing requirements, and the append-only applicability decision log.';

GRANT USAGE ON SCHEMA obligations TO zoiko_backend, authenticated;

-- ── obligations ──────────────────────────────────────────────────────────────

CREATE TABLE obligations.obligations (
    obligation_id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID         NOT NULL,

    -- Critical constraint: entity-bound and jurisdiction-bound, always.
    legal_entity_id         UUID         NOT NULL,
    jurisdiction_id         UUID         NOT NULL,

    -- Identifies the originating record (CONTRACT_CLAUSE, FILING_RULE,
    -- POLICY_MANDATE, JURISDICTION_RULE) and its id in whatever service owns
    -- that source. Not a foreign key — the source may live elsewhere entirely.
    obligation_source_type  VARCHAR(64)  NOT NULL,
    obligation_source_id    TEXT         NOT NULL,

    -- Stable human-readable identifier and the idempotent-creation dedup key.
    -- DATA ONLY, never a code switch/case.
    obligation_code         VARCHAR(128) NOT NULL,

    obligation_type         VARCHAR(64)  NOT NULL,

    -- OPEN | IN_PROGRESS | OVERDUE | CLOSED
    obligation_status       VARCHAR(32)  NOT NULL DEFAULT 'OPEN',

    due_date                TIMESTAMPTZ  NOT NULL,
    severity_level          VARCHAR(32)  NOT NULL,
    responsible_function    TEXT         NOT NULL,

    -- Atomic Linking — every obligation points to its originating source.
    source_reference        TEXT         NOT NULL,

    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL DEFAULT app.current_principal_id(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- Nil until transitioned to CLOSED; set exactly once, never unset.
    closed_at               TIMESTAMPTZ,

    CONSTRAINT obligations_status_known
        CHECK (obligation_status IN ('OPEN', 'IN_PROGRESS', 'OVERDUE', 'CLOSED')),

    -- CLOSED is terminal and stamps closed_at; the two must agree, or the
    -- register shows an obligation that is closed with no record of when.
    CONSTRAINT obligations_closed_has_timestamp
        CHECK ((obligation_status = 'CLOSED') = (closed_at IS NOT NULL)),

    CONSTRAINT obligations_source_reference_present
        CHECK (btrim(source_reference) <> '')
);

-- An obligation code is unique WITHIN a tenant. Two tenants may both have a
-- "VAT-Q1-2026" and they are different obligations.
CREATE UNIQUE INDEX idx_obligations_tenant_code_unique
    ON obligations.obligations (tenant_id, obligation_code);

CREATE INDEX idx_obligations_legal_entity ON obligations.obligations (legal_entity_id);
CREATE INDEX idx_obligations_jurisdiction ON obligations.obligations (jurisdiction_id);
CREATE INDEX idx_obligations_status       ON obligations.obligations (obligation_status);
CREATE INDEX idx_obligations_due_date     ON obligations.obligations (due_date);

-- Every read is tenant-first, so the single-column indexes above are the wrong
-- shape for the register's own queries.
CREATE INDEX idx_obligations_tenant_entity ON obligations.obligations (tenant_id, legal_entity_id);
CREATE INDEX idx_obligations_tenant_status ON obligations.obligations (tenant_id, obligation_status);
CREATE INDEX idx_obligations_tenant_due    ON obligations.obligations (tenant_id, due_date);

-- The register is read newest-first and paged; due_date alone is not a total
-- order, so the primary key rides along as the tiebreaker for stable paging.
CREATE INDEX idx_obligations_tenant_due_id
    ON obligations.obligations (tenant_id, due_date DESC, obligation_id DESC);

-- Composite key the two child tables reference, so neither can disagree with
-- its parent about which tenant owns it.
CREATE UNIQUE INDEX idx_obligations_id_tenant
    ON obligations.obligations (obligation_id, tenant_id);

-- ── filing_requirements ──────────────────────────────────────────────────────
-- Carries its own tenant_id so a query can be scoped without a join — and,
-- more importantly, so row-level security applies to it directly. A policy that
-- has to join to find its tenant is a policy that does not run on a bare SELECT.

CREATE TABLE obligations.filing_requirements (
    filing_requirement_id UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    obligation_id         UUID         NOT NULL,
    tenant_id             UUID         NOT NULL,

    filing_type           VARCHAR(64)  NOT NULL,
    filing_authority      VARCHAR(128) NOT NULL,
    submission_channel    VARCHAR(64)  NOT NULL,

    -- PENDING | SUBMITTED | ACCEPTED | REJECTED
    filing_status         VARCHAR(32)  NOT NULL DEFAULT 'PENDING',

    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT filing_requirements_status_known
        CHECK (filing_status IN ('PENDING', 'SUBMITTED', 'ACCEPTED', 'REJECTED')),

    CONSTRAINT filing_requirements_obligation_fk
        FOREIGN KEY (obligation_id, tenant_id)
        REFERENCES obligations.obligations (obligation_id, tenant_id)
);

CREATE INDEX idx_filing_requirements_obligation
    ON obligations.filing_requirements (obligation_id);
CREATE INDEX idx_filing_requirements_tenant
    ON obligations.filing_requirements (tenant_id, obligation_id);

-- ── applicability_decisions ──────────────────────────────────────────────────
-- Append-only decision log for one obligation's applicability in a given
-- jurisdiction / entity / activity / product scope. "Versioned" means new facts
-- or rules always produce a NEW row, never an UPDATE of a prior decision.
-- "Current" applicability is derived by querying the latest row whose effective
-- window covers now.
--
-- UNASSESSED (no decision has ever been made) and NOT_APPLICABLE (a decision
-- was made and concluded the obligation does not apply) are two deliberately
-- DISTINCT values. A row always represents a decision that WAS made, so
-- UNASSESSED is never stored — it is the application-level answer when no row
-- exists for a scope at all, and must never be silently coerced into
-- NOT_APPLICABLE.

CREATE TABLE obligations.applicability_decisions (
    applicability_decision_id UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    obligation_id             UUID         NOT NULL,

    -- Added by this migration; see the header.
    tenant_id                 UUID         NOT NULL,

    jurisdiction_code         VARCHAR(64)  NOT NULL,
    entity_ref                VARCHAR(255) NOT NULL,

    -- Optional finer-grained facts this decision was scoped against.
    activity_ref              VARCHAR(255),
    product_process_ref       VARCHAR(255),

    -- APPLICABLE | NOT_APPLICABLE | UNCERTAIN
    decision                  VARCHAR(32)  NOT NULL,

    -- Which approved source rule justified this decision, and its version.
    source_rule_ref           VARCHAR(255) NOT NULL,
    source_rule_version       VARCHAR(64)  NOT NULL,

    effective_from            TIMESTAMPTZ  NOT NULL,
    effective_to              TIMESTAMPTZ,

    -- The facts actually used to reach this decision. JSONB so the shape can
    -- vary by jurisdiction or obligation type without a schema change.
    facts_used                JSONB        NOT NULL DEFAULT '{}',

    confidence                NUMERIC(5,4),
    uncertainty_notes         TEXT,

    -- Exactly one of these is set: a human decided it, or a rule engine did.
    decided_by_principal_id   TEXT,
    decided_by_system         VARCHAR(128),

    -- A decision can require human review even after being reached — an
    -- UNCERTAIN outcome, or a low-confidence one.
    review_required           BOOLEAN      NOT NULL DEFAULT FALSE,

    created_at                TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT         NOT NULL DEFAULT app.current_principal_id(),

    CONSTRAINT chk_applicability_decisions_actor_or_system
        CHECK (decided_by_principal_id IS NOT NULL OR decided_by_system IS NOT NULL),

    CONSTRAINT applicability_decisions_decision_known
        CHECK (decision IN ('APPLICABLE', 'NOT_APPLICABLE', 'UNCERTAIN')),

    CONSTRAINT applicability_decisions_confidence_range
        CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),

    CONSTRAINT applicability_decisions_period_ordered
        CHECK (effective_to IS NULL OR effective_to > effective_from),

    CONSTRAINT applicability_decisions_obligation_fk
        FOREIGN KEY (obligation_id, tenant_id)
        REFERENCES obligations.obligations (obligation_id, tenant_id)
);

-- Primary lookup: current applicability for an obligation + scope, most recent
-- effective_from first.
CREATE INDEX idx_applicability_decisions_scope
    ON obligations.applicability_decisions
       (obligation_id, jurisdiction_code, entity_ref, effective_from DESC);
CREATE INDEX idx_applicability_decisions_tenant
    ON obligations.applicability_decisions (tenant_id, obligation_id);

CREATE TRIGGER applicability_decisions_immutable
    BEFORE UPDATE OR DELETE ON obligations.applicability_decisions
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE obligations.obligations              ENABLE ROW LEVEL SECURITY;
ALTER TABLE obligations.obligations              FORCE  ROW LEVEL SECURITY;
ALTER TABLE obligations.filing_requirements      ENABLE ROW LEVEL SECURITY;
ALTER TABLE obligations.filing_requirements      FORCE  ROW LEVEL SECURITY;
ALTER TABLE obligations.applicability_decisions  ENABLE ROW LEVEL SECURITY;
ALTER TABLE obligations.applicability_decisions  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON obligations.obligations
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON obligations.obligations
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_isolation ON obligations.filing_requirements
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON obligations.filing_requirements
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_isolation ON obligations.applicability_decisions
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON obligations.applicability_decisions
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON obligations.obligations             TO authenticated;
GRANT SELECT ON obligations.filing_requirements     TO authenticated;
GRANT SELECT ON obligations.applicability_decisions TO authenticated;

-- No DELETE on any of the three: an obligation is CLOSED, never removed.
GRANT SELECT, INSERT, UPDATE ON obligations.obligations         TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON obligations.filing_requirements TO zoiko_backend;
GRANT SELECT, INSERT         ON obligations.applicability_decisions TO zoiko_backend;
