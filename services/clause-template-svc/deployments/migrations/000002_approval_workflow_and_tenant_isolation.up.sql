-- +migrate Up
BEGIN;

-- LEG-06 (docs/architecture/original_doc) §8 names a 5-state lifecycle
-- (Draft -> Legal Review -> Approved -> Active -> Retired/Superseded) and
-- commands CreateClause/CreateClauseVersion/ApproveClause/RetireClause/
-- ApproveDeviationRule. This service had none of it: status was DRAFT,
-- ACTIVE or ARCHIVED, and no code path ever transitioned a clause away from
-- DRAFT — UpdateClause mutated the row in place forever, with no version
-- history and no approval gate. §8.1's own invariants ("LLM-generated draft
-- language can never be promoted to approved clause status without human
-- legal review", "clause deviations require risk/playbook classification
-- and accountable approval") were unenforceable because nothing here
-- implemented promotion or deviation at all.
ALTER TABLE clauses DROP CONSTRAINT IF EXISTS clauses_status_known;
ALTER TABLE clauses
    ADD CONSTRAINT clauses_status_known
    CHECK (status IN ('DRAFT','LEGAL_REVIEW','APPROVED','ACTIVE','RETIRED','SUPERSEDED')) NOT VALID;

ALTER TABLE clauses ADD COLUMN IF NOT EXISTS authored_by_ai BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS submitted_by TEXT;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS approved_by TEXT;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS activated_at TIMESTAMPTZ;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS activated_by TEXT;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS retired_at TIMESTAMPTZ;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS retired_by TEXT;
ALTER TABLE clauses ADD COLUMN IF NOT EXISTS superseded_by TEXT;

-- CreateClauseVersion's own evidence trail — append-only, same pattern as
-- contract-lifecycle-svc's contract_versions. Historical contracts retain
-- the clause version they actually executed (§8.1); that only means
-- something if a version history actually exists to retain.
CREATE TABLE IF NOT EXISTS clause_versions (
    version_id      TEXT        NOT NULL PRIMARY KEY,
    clause_id       TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    version_number  INTEGER     NOT NULL,
    status          TEXT        NOT NULL,
    title           TEXT        NOT NULL,
    body            TEXT        NOT NULL,
    jurisdiction_id TEXT        NOT NULL,
    effective_from  DATE        NOT NULL,
    effective_to    DATE,
    change_summary  TEXT        NOT NULL DEFAULT '',
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Maker-checker, accountable approval (LEG-06 §8's Authorization/SoD and
-- §8.1's deviation invariant): one row per proposed deviation, requiring a
-- distinct approver from the proposer, same two-layer pattern (handler +
-- locked-row re-check) used across this codebase's other SoD gates.
CREATE TABLE IF NOT EXISTS clause_deviation_rules (
    deviation_id       TEXT        NOT NULL PRIMARY KEY,
    tenant_id          TEXT        NOT NULL,
    legal_entity_id    TEXT        NOT NULL,
    clause_id          TEXT,
    jurisdiction_id    TEXT        NOT NULL,
    risk_classification TEXT       NOT NULL,
    description        TEXT        NOT NULL,
    status             TEXT        NOT NULL DEFAULT 'PROPOSED',
    proposed_by        TEXT        NOT NULL,
    proposed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by        TEXT,
    approved_at        TIMESTAMPTZ,
    CONSTRAINT clause_deviation_rules_risk_known
        CHECK (risk_classification IN ('LOW','MEDIUM','HIGH','CRITICAL')),
    CONSTRAINT clause_deviation_rules_status_known
        CHECK (status IN ('PROPOSED','APPROVED','REJECTED'))
);

ALTER TABLE clause_deviation_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE clause_deviation_rules FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS deviation_rules_tenant_isolation ON clause_deviation_rules;
CREATE POLICY deviation_rules_tenant_isolation ON clause_deviation_rules
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_clause_versions_clause_id ON clause_versions (clause_id, tenant_id);
CREATE INDEX IF NOT EXISTS idx_deviation_rules_tenant_entity ON clause_deviation_rules (tenant_id, legal_entity_id);

-- Templates: TemplateApproved is a named canonical event (§8's own table),
-- so templates get the same DRAFT -> ACTIVE approval gate as clauses,
-- without the clause-only LEGAL_REVIEW/RETIRED/SUPERSEDED intermediate
-- states the spec does not name for templates.
ALTER TABLE contract_templates DROP CONSTRAINT IF EXISTS contract_templates_status_known;
ALTER TABLE contract_templates
    ADD CONSTRAINT contract_templates_status_known
    CHECK (status IN ('DRAFT','ACTIVE','ARCHIVED')) NOT VALID;
ALTER TABLE contract_templates ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE contract_templates ADD COLUMN IF NOT EXISTS approved_by TEXT;

-- Tenant isolation: this table's RLS policy (migration 000001) was ENABLE
-- only, and every query in the store ran with no explicit tenant_id
-- predicate of its own. This pool connects as the Postgres superuser/table
-- owner (same as every other service on this platform), and Postgres
-- unconditionally exempts a table's owner from RLS unless FORCE ROW LEVEL
-- SECURITY is set. So this policy never applied to a single query this
-- service ever made: GetClause/UpdateClause/GetTemplate/UpdateTemplate all
-- read and wrote by id alone, across every tenant in the database. FORCE
-- closes that; the store's own explicit tenant_id predicates (added
-- alongside this migration) are the belt to this policy's braces, the same
-- defence-in-depth already applied in board-resolutions-svc and
-- contract-lifecycle-svc after the identical bug was found live in each.
ALTER TABLE clauses FORCE ROW LEVEL SECURITY;
ALTER TABLE contract_templates FORCE ROW LEVEL SECURITY;

COMMIT;
