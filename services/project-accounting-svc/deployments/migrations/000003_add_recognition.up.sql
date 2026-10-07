-- Migration: 000003_add_recognition.up.sql
--
-- PRJ-03 (Project Revenue & WIP): "owns ProjectRecognitionRun. Must never
-- own: Invoice issuance, cash settlement or direct GL posting [beyond its
-- own recognition accounting event]." Fuller ownership (verbatim):
-- "ProjectRecognitionRun; performance obligation/milestone mapping;
-- measure of progress; recognized revenue/cost/margin; WIP/unbilled/
-- contract asset/liability/deferred balance; estimate version;
-- adjustment chain." Purpose (verbatim): "Calculate project/job revenue
-- recognition, WIP and contract-balance facts under approved accounting-
-- framework policy while keeping billing, cash and accounting posting
-- authorities separate." The doc's own authority-matrix cross-reference
-- states the whole design intent in one line: "AR owns issued invoices;
-- PRJ-03 determines recognition/WIP/contract balance under approved
-- policy; cash settlement remains Banking." "Billing/cash receipt never
-- equals revenue by default" (the doc's own failure semantics) is this
-- capability's entire reason to exist.
--
-- State model (verbatim): "Draft→PopulationFrozen→Calculated→Reviewed→
-- Approved→AccountingEventEmitted→Reconciled/Certified; estimate/
-- revision supersedes, never overwrites certified run." The same
-- doc-vs-command mismatch pattern recurring throughout this build hits
-- PRJ-03 too: no command reaches Reconciled/Certified independently —
-- left unreachable, the same posture as AST-02's own gap. CalculateProjectRevenue
-- and CalculateProjectWIP are two named commands feeding the SAME
-- Calculated state — this v1 runs them together as one real calculation
-- (percentage-of-completion cost-to-cost), the same collapse
-- ValidateDepreciationRun already used for AST-02's own Run.Calculated.
--
-- Real percentage-of-completion calculation, using REAL PRJ-02 cost data
-- in-process (same service, same database — the same cross-capability
-- pattern used throughout this sub-domain): % complete = ITD cost
-- incurred (summed live from project_cost_entries at freeze time) /
-- (ITD cost incurred + estimate-to-complete, from the current SetApprovedEstimate
-- version). Cumulative recognized revenue = % complete × contract_value.
-- Contract value and billed-to-date are caller-declared bootstrap inputs
-- — no BIZ contract service or live AR integration exists on this
-- platform yet, the same "deliberate bootstrap gap, safety-favoring
-- direction" posture used throughout this session.
--
-- Minimum negative-path certification (verbatim, all four):
--   1. "Invoice amount treated automatically as revenue" — this service
--      has no AR client anywhere in this v1; recognized revenue is
--      always CALCULATED from cost-to-cost progress against a
--      caller-declared contract_value, never copied from a billing
--      figure. billed_to_date is captured purely as a comparison input
--      for the WIP/contract-balance classification, never as the
--      revenue figure itself.
--   2. "Progress estimate changed after approval without invalidation"
--      — project_recognition_estimates is versioned exactly like PRJ-01's
--      own project_financial_profiles: SetApprovedEstimate only ever
--      accepts a future-effective version. Each run additionally SNAPSHOTS
--      the estimate value it actually used (estimate_to_complete, a real
--      column on the run itself, frozen once the run reaches CALCULATED
--      — see the run's own reject-mutation trigger below) — a later
--      estimate revision can never rewrite an already-calculated run's
--      own historical numbers.
--   3. "Contract asset silently classified as inventory WIP" — this
--      service's own schema has no table, column, or code path that
--      could ever write to inventory-management-svc's own database —
--      cross-service writes on this platform only ever happen through
--      that service's own HTTP API, and this migration defines none.
--      balance_type is additionally a closed CHECK-constrained enum
--      (CONTRACT_ASSET/CONTRACT_LIABILITY/NONE) with no INVENTORY-shaped
--      value in it at all.
--   4. "Recognition rerun duplicates GL posting" — the real
--      UNIQUE(tenant_id, project_id, fiscal_period) WHERE status !=
--      'SUPERSEDED' partial index (mirrors AST-02's own DepreciationRun
--      and INV-04's own ValuationRun exactly) makes a second live run
--      for the same project/period structurally impossible; SupersedeRecognitionRun
--      is the only way to release that slot, and — like every other
--      Supersede command this session — reverses the prior run's own
--      journal first.
--
-- project_recognition_runs is append-only once CALCULATED (its own
-- calculated figures — percent_complete, revenue, cost, margin, balance
-- — never change again) but its own approval/emission metadata still
-- walks the state model forward, the same immutability shape AST-03's
-- own asset_events and PRJ-02's own cost_entries already use.
CREATE TABLE project_recognition_estimates (
    estimate_version_id      UUID PRIMARY KEY,
    estimate_id                 UUID NOT NULL,
    version                       INT NOT NULL,
    tenant_id                      VARCHAR(255) NOT NULL,
    project_id                       UUID NOT NULL REFERENCES projects(project_id),
    estimate_to_complete                NUMERIC(18,2) NOT NULL,
    effective_from                        TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to                            TIMESTAMP WITH TIME ZONE, -- NULL = current
    created_at                                TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                     VARCHAR(255) NOT NULL
);

CREATE UNIQUE INDEX idx_recognition_estimates_current
    ON project_recognition_estimates (tenant_id, project_id) WHERE effective_to IS NULL;

CREATE TABLE project_recognition_runs (
    run_id                       UUID PRIMARY KEY,
    tenant_id                       VARCHAR(255) NOT NULL,
    legal_entity_id                    VARCHAR(255) NOT NULL,
    project_id                            UUID NOT NULL REFERENCES projects(project_id),
    fiscal_period                            VARCHAR(20) NOT NULL,
    status                                      VARCHAR(30) NOT NULL, -- DRAFT|POPULATION_FROZEN|CALCULATED|REVIEWED|APPROVED|ACCOUNTING_EVENT_EMITTED|SUPERSEDED
    contract_value                                NUMERIC(18,2), -- caller-declared; "transaction price" — no BIZ contract service exists yet
    billed_to_date                                  NUMERIC(18,2), -- caller-declared; no live AR integration exists yet
    estimate_to_complete                              NUMERIC(18,2), -- snapshotted from the current SetApprovedEstimate version at freeze time
    itd_cost_incurred                                   NUMERIC(18,2), -- summed live from project_cost_entries at freeze time
    percent_complete                                      NUMERIC(9,6),
    cumulative_recognized_revenue                           NUMERIC(18,2),
    period_recognized_revenue                                 NUMERIC(18,2),
    recognized_cost                                             NUMERIC(18,2),
    margin                                                        NUMERIC(18,2),
    balance_type                                                    VARCHAR(30), -- CONTRACT_ASSET|CONTRACT_LIABILITY|NONE
    balance_amount                                                    NUMERIC(18,2),
    revenue_account_code                                                VARCHAR(64), -- caller-supplied, same bootstrap posture as AST-02/INV-04's own run account codes
    wip_account_code                                                      VARCHAR(64),
    journal_id                                                              VARCHAR(255),
    supersedes_run_id                                                         UUID REFERENCES project_recognition_runs(run_id),
    superseded_by_run_id                                                        UUID REFERENCES project_recognition_runs(run_id),
    created_at                                                                    TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                                                         VARCHAR(255) NOT NULL,
    frozen_at                                                                         TIMESTAMP WITH TIME ZONE,
    calculated_at                                                                       TIMESTAMP WITH TIME ZONE,
    validated_at                                                                          TIMESTAMP WITH TIME ZONE,
    approved_at                                                                             TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id                                                                  VARCHAR(255),
    emitted_at                                                                                  TIMESTAMP WITH TIME ZONE,
    superseded_at                                                                                 TIMESTAMP WITH TIME ZONE,
    superseded_by_principal_id                                                                      VARCHAR(255),

    CONSTRAINT chk_recognition_run_status CHECK (status IN (
        'DRAFT', 'POPULATION_FROZEN', 'CALCULATED', 'REVIEWED', 'APPROVED', 'ACCOUNTING_EVENT_EMITTED', 'SUPERSEDED'
    )),
    CONSTRAINT chk_recognition_run_balance_type CHECK (balance_type IS NULL OR balance_type IN ('CONTRACT_ASSET', 'CONTRACT_LIABILITY', 'NONE'))
);

CREATE UNIQUE INDEX idx_recognition_runs_live_period
    ON project_recognition_runs (tenant_id, project_id, fiscal_period) WHERE status != 'SUPERSEDED';

-- Negative path #2's own structural backbone — once a run has been
-- CALCULATED, its own calculated figures are immutable; only
-- status/approval/emission/supersession metadata may change afterward.
CREATE OR REPLACE FUNCTION reject_recognition_run_calculation_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status NOT IN ('DRAFT', 'POPULATION_FROZEN') AND (
        NEW.contract_value IS DISTINCT FROM OLD.contract_value OR
        NEW.billed_to_date IS DISTINCT FROM OLD.billed_to_date OR
        NEW.estimate_to_complete IS DISTINCT FROM OLD.estimate_to_complete OR
        NEW.itd_cost_incurred IS DISTINCT FROM OLD.itd_cost_incurred OR
        NEW.percent_complete IS DISTINCT FROM OLD.percent_complete OR
        NEW.cumulative_recognized_revenue IS DISTINCT FROM OLD.cumulative_recognized_revenue OR
        NEW.period_recognized_revenue IS DISTINCT FROM OLD.period_recognized_revenue OR
        NEW.recognized_cost IS DISTINCT FROM OLD.recognized_cost OR
        NEW.margin IS DISTINCT FROM OLD.margin OR
        NEW.balance_type IS DISTINCT FROM OLD.balance_type OR
        NEW.balance_amount IS DISTINCT FROM OLD.balance_amount
    ) THEN
        RAISE EXCEPTION 'project_recognition_runs: calculated figures are immutable once CALCULATED — use SupersedeRecognitionRun, not UPDATE';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_recognition_run_calculation_mutation
    BEFORE UPDATE ON project_recognition_runs
    FOR EACH ROW EXECUTE FUNCTION reject_recognition_run_calculation_mutation();

ALTER TABLE project_recognition_estimates ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_recognition_estimates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_recognition_estimates
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE project_recognition_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_recognition_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_recognition_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_recognition_estimates_project ON project_recognition_estimates (tenant_id, project_id);
CREATE INDEX idx_recognition_runs_project ON project_recognition_runs (tenant_id, project_id);
