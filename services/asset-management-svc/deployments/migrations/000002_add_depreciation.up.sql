-- Migration: 000002_add_depreciation.up.sql
--
-- AST-02 (Depreciation): "owns DepreciationSchedule. Must never own:
-- Asset physical identity, impairment/disposal decisions or direct GL
-- posting." Fuller ownership: "DepreciationSchedule; DepreciationRun;
-- DepreciationLine; accumulated-depreciation subledger facts by
-- asset/component/book; run eligibility/exceptions; schedule version."
--
-- TWO independently-lifecycled state machines, verbatim from spec:
-- "Schedule Draft→Validated→Active→Superseded; Run Draft→
-- PopulationFrozen→Calculated→Validated→Approved→
-- AccountingEventEmitted→Reconciled/Certified."
--
-- Named commands (BuildDepreciationSchedule, RecalculateSchedule,
-- CreateDepreciationRun, FreezeDepreciationPopulation,
-- ValidateDepreciationRun, ApproveDepreciationRun,
-- EmitDepreciationAccountingEvent, SupersedeDepreciationRun) don't cover
-- every named state 1:1 — the same doc-internal pattern already found
-- across ACC-08/09/10/17 and AST-01 in this platform:
--   * No command reaches Schedule's own Draft/Validated independently of
--     Active — BuildDepreciationSchedule lands a schedule directly in
--     ACTIVE, the same "no separate command exists for a named
--     intermediate state" collapse as AST-01's Candidate/Reviewed.
--   * No command reaches Run's own Calculated independently of Validated
--     — ValidateDepreciationRun performs the actual calculation AND
--     validates it in one step, the same collapse ACC-10's own
--     Planned/Calculated used.
--   * No command reaches Run's own Reconciled/Certified at all — left
--     unbuilt in this v1, stated honestly, same posture as ACC-16's own
--     gaps.
--   * SupersedeDepreciationRun is named as a RUN command even though the
--     Run state model itself never lists "Superseded" — only the
--     SCHEDULE does. Real intent, inferred from the spec's own negative
--     path "Rerun emits duplicate accounting event": reversing an
--     already-emitted run's journal and marking that run SUPERSEDED
--     (added to the Run's own status set here) so a caller can create a
--     fresh run for the same period without recomputing history —
--     mirrors ACC-10 FX Revaluation's own "reversed via new run" pattern.
--
-- depreciation_schedules is versioned exactly like ACC-09's own
-- AllocationRule (migration 000007 in financial-close-svc): a stable
-- logical schedule_id carrying effective-dated versions, never mutated
-- in place. RecalculateSchedule creates a new version and end-dates the
-- old one — the spec's own negative path, "Useful life changed after
-- approval without invalidation," is satisfied structurally: there is no
-- in-place edit path to a schedule's own parameters, only supersession.
-- UNIQUE(tenant_id, asset_id, book_id) WHERE effective_to IS NULL is the
-- real, database-enforced fix for "Same asset depreciated twice in
-- period" at its root: an asset/book pair can have at most one CURRENT
-- schedule, so at most one run can ever draw a depreciation line from it
-- for a given period.
CREATE TABLE depreciation_schedules (
    schedule_version_id       UUID PRIMARY KEY,
    schedule_id                 UUID NOT NULL,
    version                      INT NOT NULL,
    tenant_id                     VARCHAR(255) NOT NULL,
    legal_entity_id                 VARCHAR(255) NOT NULL,
    asset_id                          UUID NOT NULL REFERENCES fixed_assets(asset_id),
    book_id                            VARCHAR(255) NOT NULL,
    method                               VARCHAR(30) NOT NULL DEFAULT 'STRAIGHT_LINE', -- only method implemented in this v1
    cost_basis                           NUMERIC(18,2) NOT NULL,
    residual_value                        NUMERIC(18,2) NOT NULL DEFAULT 0,
    useful_life_months                     INT NOT NULL,
    in_service_date                         DATE NOT NULL,
    status                                   VARCHAR(20) NOT NULL, -- ACTIVE|SUPERSEDED (DRAFT/VALIDATED named by spec, unreachable in this v1 — see doc comment above)
    effective_to                             TIMESTAMP WITH TIME ZONE, -- NULL = current version
    created_at                                TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                    VARCHAR(255) NOT NULL,

    CONSTRAINT chk_depreciation_schedule_status CHECK (status IN ('ACTIVE', 'SUPERSEDED')),
    CONSTRAINT chk_depreciation_schedule_method CHECK (method IN ('STRAIGHT_LINE'))
);

CREATE UNIQUE INDEX idx_depreciation_schedules_current_version
    ON depreciation_schedules (tenant_id, schedule_id) WHERE effective_to IS NULL;

CREATE UNIQUE INDEX idx_depreciation_schedules_current_asset_book
    ON depreciation_schedules (tenant_id, asset_id, book_id) WHERE effective_to IS NULL;

-- depreciation_runs is a normal mutable stateful row walking the Run
-- state model. UNIQUE(...) WHERE status != 'SUPERSEDED' is the real
-- enforcement of "at most one live run per (entity, period)" while still
-- allowing SupersedeDepreciationRun to open the period back up for a
-- fresh run once the prior one is explicitly superseded.
CREATE TABLE depreciation_runs (
    run_id                      UUID PRIMARY KEY,
    tenant_id                     VARCHAR(255) NOT NULL,
    legal_entity_id                 VARCHAR(255) NOT NULL,
    fiscal_period                     VARCHAR(20) NOT NULL,
    depreciation_expense_account_code  VARCHAR(64) NOT NULL,
    accumulated_depreciation_account_code VARCHAR(64) NOT NULL,
    status                                  VARCHAR(30) NOT NULL, -- DRAFT|POPULATION_FROZEN|VALIDATED|APPROVED|ACCOUNTING_EVENT_EMITTED|SUPERSEDED
    journal_id                              VARCHAR(255),
    supersedes_run_id                        UUID REFERENCES depreciation_runs(run_id),
    superseded_by_run_id                      UUID REFERENCES depreciation_runs(run_id),
    created_at                                 TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                     VARCHAR(255) NOT NULL,
    frozen_at                                    TIMESTAMP WITH TIME ZONE,
    validated_at                                  TIMESTAMP WITH TIME ZONE,
    approved_at                                    TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id                        VARCHAR(255),
    emitted_at                                       TIMESTAMP WITH TIME ZONE,
    superseded_at                                     TIMESTAMP WITH TIME ZONE,
    superseded_by_principal_id                         VARCHAR(255),

    CONSTRAINT chk_depreciation_run_status CHECK (status IN (
        'DRAFT', 'POPULATION_FROZEN', 'VALIDATED', 'APPROVED', 'ACCOUNTING_EVENT_EMITTED', 'SUPERSEDED'
    ))
);

CREATE UNIQUE INDEX idx_depreciation_runs_live_period
    ON depreciation_runs (tenant_id, legal_entity_id, fiscal_period) WHERE status != 'SUPERSEDED';

-- depreciation_run_population is the spec's own named evidence, "frozen
-- population manifest" — exactly which schedule versions
-- FreezeDepreciationPopulation locked in, so ValidateDepreciationRun's
-- own calculation reads ONLY this frozen set, never a live re-query that
-- could silently include a schedule created after the freeze.
CREATE TABLE depreciation_run_population (
    run_id                UUID NOT NULL REFERENCES depreciation_runs(run_id),
    schedule_version_id     UUID NOT NULL REFERENCES depreciation_schedules(schedule_version_id),
    PRIMARY KEY (run_id, schedule_version_id)
);

-- depreciation_lines is append-only calculation evidence — one row per
-- (run, schedule_version), never mutated. UNIQUE(run_id,
-- schedule_version_id) is the same-asset-twice-in-one-run guard at the
-- line level, on top of the schedule-level and run-level guards above.
CREATE TABLE depreciation_lines (
    line_id                  UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    run_id                       UUID NOT NULL REFERENCES depreciation_runs(run_id),
    schedule_version_id            UUID NOT NULL REFERENCES depreciation_schedules(schedule_version_id),
    asset_id                         UUID NOT NULL REFERENCES fixed_assets(asset_id),
    book_id                            VARCHAR(255) NOT NULL,
    period_amount                       NUMERIC(18,2) NOT NULL,
    accumulated_depreciation_after        NUMERIC(18,2) NOT NULL,
    created_at                              TIMESTAMP WITH TIME ZONE NOT NULL,

    UNIQUE (run_id, schedule_version_id)
);

CREATE OR REPLACE FUNCTION reject_depreciation_line_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'depreciation_lines is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_depreciation_line_update
    BEFORE UPDATE ON depreciation_lines
    FOR EACH ROW EXECUTE FUNCTION reject_depreciation_line_mutation();
CREATE TRIGGER trg_reject_depreciation_line_delete
    BEFORE DELETE ON depreciation_lines
    FOR EACH ROW EXECUTE FUNCTION reject_depreciation_line_mutation();

ALTER TABLE depreciation_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE depreciation_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON depreciation_schedules
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE depreciation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE depreciation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON depreciation_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE depreciation_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE depreciation_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON depreciation_lines
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_depreciation_schedules_entity ON depreciation_schedules (tenant_id, legal_entity_id);
CREATE INDEX idx_depreciation_runs_entity ON depreciation_runs (tenant_id, legal_entity_id);
CREATE INDEX idx_depreciation_lines_run ON depreciation_lines (tenant_id, run_id);
