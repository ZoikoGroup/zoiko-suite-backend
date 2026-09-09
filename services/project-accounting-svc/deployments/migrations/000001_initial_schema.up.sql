-- Migration: 000001_initial_schema.up.sql
--
-- PRJ-01 (Project / Job Master): "owns FinancialProject. Must never own:
-- General task/project-management execution or cross-entity accounting
-- aggregate." Fuller ownership (verbatim): "FinancialProject;
-- ProjectWorkPackage/WBS; legal entity; customer/contract references;
-- project type; manager/cost center; accounting/billing policy refs;
-- dimensions; lifecycle." Purpose (verbatim): "Maintain the financial
-- project/job identity, WBS/work-package structure and accounting-policy
-- bindings required for project costing and revenue/WIP without becoming
-- a general project-management task system."
--
-- State model (verbatim): "Draft→Approved→Active→Suspended→Closing→
-- Closed; Reopened controlled. WBS versions effective-dated." The same
-- doc-vs-command mismatch pattern recurring throughout this build hits
-- PRJ-01 too: no command reaches "Closing" independently of CloseProject
-- itself — the same collapse AST-01's own ApproveAssetRegistration
-- already used for Candidate/Reviewed. SuspendProject has no reverse
-- command anywhere in the spec's own command list — SUSPENDED is a dead
-- end in this v1, the same posture INV-02's own SuspendLocation already
-- took. ReopenProjectControlled is real and distinct — it is the
-- CLOSED → ACTIVE bridge, never a bridge out of SUSPENDED.
--
-- Minimum negative-path certification (verbatim, all four):
--   1. "Single project posts costs across two legal entities" — a
--      project has exactly one legal_entity_id column, set once at
--      CreateProject and never editable by any later command (not even
--      AmendFinancialProfile) — there is no schema path for a second
--      entity to ever attach to one project. "Group programs use
--      separate entity projects with group reference" is satisfied by
--      group_reference: a caller-declared, evidence-only correlation key
--      across separately-owned, single-entity projects — never a shared
--      accounting aggregate. The real cost-posting half of this negative
--      path belongs to PRJ-02 (not yet built), which will validate every
--      captured cost's own source entity against this column — stated
--      honestly as deferred, the same posture INV-01/02 took for
--      negative paths later capabilities in the same sub-domain finish.
--   2. "Recognition policy changed after run without invalidation" —
--      project_financial_profiles is versioned exactly like INV-01's own
--      ValuationPolicy: AmendFinancialProfile only ever accepts an
--      effective_from strictly later than the moment it runs (enforced
--      at the handler layer, mirroring SetValuationPolicyFutureEffective
--      exactly) — there is no in-place edit path to a profile's own
--      recognition_method that could retroactively rewrite what a past
--      PRJ-03 revenue run (not yet built) already calculated against.
--   3. "Closed project accepts new cost without controlled reopen" —
--      CLOSED is only ever reached via CloseProject and only ever left
--      via ReopenProjectControlled, which refuses self-reopen (the
--      spec's own SoD: "closed project reopen requires independent
--      approval") — the same maker/checker posture used throughout this
--      session. PRJ-02's own future cost-capture commands will check
--      this project's own status is ACTIVE before accepting a cost,
--      mirroring how INV-03 checks INV-01/02's own status.
--   4. "WBS deletion orphans historical cost" — there is no delete (or
--      even amend) command for a work package anywhere in the spec's own
--      command list; AddWorkPackage is the only write path, and this
--      migration's own trg_reject_wbs_mutation makes every work package
--      row permanently immutable once created — structurally impossible
--      to delete or orphan, not merely undocumented.
--
-- project_financial_profiles is versioned exactly like INV-01's own
-- inventory_valuation_policies (migration 000001 in inventory-management-svc)
-- and AST-02's own DepreciationSchedule — a stable logical profile_id
-- carrying effective-dated versions, never mutated in place.
CREATE TABLE projects (
    project_id                UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id                 VARCHAR(255) NOT NULL, -- immutable after creation — see negative path #1
    project_code                      VARCHAR(100) NOT NULL,
    name                                 TEXT NOT NULL,
    project_type                          VARCHAR(50),
    customer_ref                            VARCHAR(255), -- caller-declared; BIZ contract/customer refs do not exist platform-wide yet
    contract_ref                              VARCHAR(255), -- set via LinkContract
    manager_principal_id                        VARCHAR(255),
    cost_center                                   VARCHAR(100),
    group_reference                                 VARCHAR(255), -- "group programs use separate entity projects with group reference"
    start_date                                        DATE,
    end_date                                            DATE,
    status                                                VARCHAR(20) NOT NULL, -- DRAFT|APPROVED|ACTIVE|SUSPENDED|CLOSED (CLOSING unreachable in this v1 — see doc comment above)
    created_at                                             TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                                  VARCHAR(255) NOT NULL,
    approved_at                                                TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id                                     VARCHAR(255),
    activated_at                                                   TIMESTAMP WITH TIME ZONE,
    suspended_at                                                     TIMESTAMP WITH TIME ZONE,
    suspended_by_principal_id                                          VARCHAR(255),
    suspension_reason                                                    TEXT,
    closed_at                                                              TIMESTAMP WITH TIME ZONE,
    closed_by_principal_id                                                   VARCHAR(255),
    close_reason                                                               TEXT,
    reopened_at                                                                  TIMESTAMP WITH TIME ZONE,
    reopened_by_principal_id                                                       VARCHAR(255),
    reopen_reason                                                                    TEXT,

    CONSTRAINT chk_project_status CHECK (status IN ('DRAFT', 'APPROVED', 'ACTIVE', 'SUSPENDED', 'CLOSED')),
    UNIQUE (tenant_id, legal_entity_id, project_code)
);

-- Negative path #4, "WBS deletion orphans historical cost" — append-only,
-- reject-mutation-guarded evidence, the same pattern AST-02's own
-- depreciation_lines and INV-03's own committed movements use.
CREATE TABLE project_work_packages (
    wbs_id                UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    project_id                  UUID NOT NULL REFERENCES projects(project_id),
    wbs_code                       VARCHAR(100) NOT NULL,
    description                       TEXT,
    parent_wbs_id                        UUID REFERENCES project_work_packages(wbs_id),
    effective_from                          TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at                                 TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                      VARCHAR(255) NOT NULL,

    UNIQUE (tenant_id, project_id, wbs_code)
);

CREATE OR REPLACE FUNCTION reject_wbs_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'project_work_packages is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_wbs_update
    BEFORE UPDATE ON project_work_packages
    FOR EACH ROW EXECUTE FUNCTION reject_wbs_mutation();
CREATE TRIGGER trg_reject_wbs_delete
    BEFORE DELETE ON project_work_packages
    FOR EACH ROW EXECUTE FUNCTION reject_wbs_mutation();

CREATE TABLE project_financial_profiles (
    profile_version_id      UUID PRIMARY KEY,
    profile_id                 UUID NOT NULL,
    version                      INT NOT NULL,
    tenant_id                      VARCHAR(255) NOT NULL,
    project_id                       UUID NOT NULL REFERENCES projects(project_id),
    recognition_method                 VARCHAR(30) NOT NULL,
    billing_type                         VARCHAR(30) NOT NULL,
    currency                               VARCHAR(3) NOT NULL,
    effective_from                           TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to                               TIMESTAMP WITH TIME ZONE, -- NULL = current
    created_at                                   TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                        VARCHAR(255) NOT NULL,

    CONSTRAINT chk_recognition_method CHECK (recognition_method IN ('PERCENTAGE_OF_COMPLETION', 'COMPLETED_CONTRACT', 'TIME_AND_MATERIALS')),
    CONSTRAINT chk_billing_type CHECK (billing_type IN ('FIXED_PRICE', 'TIME_AND_MATERIALS', 'COST_PLUS'))
);

CREATE UNIQUE INDEX idx_financial_profiles_current
    ON project_financial_profiles (tenant_id, project_id) WHERE effective_to IS NULL;

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON projects
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE project_work_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_work_packages FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_work_packages
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE project_financial_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_financial_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_financial_profiles
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_projects_entity ON projects (tenant_id, legal_entity_id);
CREATE INDEX idx_wbs_project ON project_work_packages (tenant_id, project_id);
CREATE INDEX idx_financial_profiles_project ON project_financial_profiles (tenant_id, project_id);
