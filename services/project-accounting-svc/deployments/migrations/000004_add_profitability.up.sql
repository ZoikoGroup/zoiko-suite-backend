-- Migration: 000004_add_profitability.up.sql
--
-- PRJ-04 (Project Profitability): "owns ProjectProfitabilitySnapshot/read
-- model. Must never own: Revenue recognition, cost mutation, WIP decisions
-- or accounting authority." Purpose (verbatim): "Provide reconciled,
-- freshness-labeled project profitability and performance analytics from
-- authoritative project cost/revenue/WIP and accounting facts without
-- becoming a posting or recognition authority." Domain invariant
-- (verbatim): "Project profitability is a read model | PRJ-04 cannot
-- recognize revenue, post costs, edit WIP or change project lifecycle."
--
-- This is the fourth and final Project Accounting capability, and the
-- first purely read-model capability in this service — mirrors ACC-15's
-- Trial Balance / ACC-18's lineage projection pattern used elsewhere on
-- this platform: a reconciled projection over already-authoritative
-- facts, never a source of new ones.
--
-- State model (verbatim): "Projection Current/Stale/Rebuilding; Snapshot
-- Draft→Reconciled→Certified; no business lifecycle authority." Two
-- design notes, both following precedent already set earlier in this
-- build:
--   1. REBUILDING is not independently observable in this v1 — a refresh
--      is a single-transaction, synchronous recompute that either fully
--      succeeds (landing in CURRENT) or fully fails (leaving the prior
--      row untouched); there is no long-running background rebuild to
--      expose a transient REBUILDING state for. Stated honestly rather
--      than inventing an unobservable status flicker.
--   2. Wireframe commands (verbatim): "BuildProfitabilitySnapshot;
--      RefreshProfitabilityProjection; CertifyProfitabilitySnapshot;
--      RebuildProjection." No command reaches a standalone DRAFT snapshot
--      independently of reconciling it — building a snapshot from the
--      live projection IS the act of reconciling it against source, so
--      BuildProfitabilitySnapshot lands directly in RECONCILED, the same
--      doc-vs-command collapse used throughout this build (e.g.
--      ValidateDepreciationRun/AST-02, FreezeAndCalculate/PRJ-03).
--      RefreshProfitabilityProjection and RebuildProjection also collapse
--      into one real recompute — this v1 has no incremental cache to
--      distinguish an incremental refresh from a full rebuild.
--
-- Minimum negative-path certification (verbatim, all four):
--   1. "Stale project margin shown as certified" — CertifyProfitabilitySnapshot
--      re-verifies the snapshot's own frozen watermarks against the LIVE
--      source data (project_cost_entries, project_recognition_runs) at
--      certify time, not just at build time — if anything newer has
--      landed since the snapshot was built, certification is refused.
--      BuildProfitabilitySnapshot itself also refuses to build from a
--      projection already known to be STALE.
--   2. "Manual edit changes margin without source fact" — there is no
--      UPDATE endpoint/command anywhere for a snapshot's own revenue/
--      cost/margin/billed/unbilled figures; the reject-mutation trigger
--      below makes any such UPDATE structurally impossible once a
--      snapshot row exists, the same immutability shape used throughout
--      this build.
--   3. "Profitability snapshot omits source watermark" — cost_watermark_at
--      is NOT NULL; revenue_watermark_at is only NULL when the project
--      genuinely has no recognition run yet (a real, valid state, not a
--      missing one) — both are always populated FROM a real, live query
--      against source tables at build time, never left as an optional
--      caller-declared field.
--   4. "Analytics write changes recognition state" — this migration
--      defines no write path into project_recognition_runs or
--      project_cost_entries at all; PRJ-04's own store methods only ever
--      SELECT from those tables (in-process, same database — the same
--      cross-capability read pattern used throughout this sub-domain) and
--      INSERT/UPDATE only their own two tables below.
--
-- SoD (verbatim): "certification requires reconciliation owner separate
-- from metric-model author where configured." No metric-model-authorship
-- registration exists anywhere on this platform — the same "deliberate
-- bootstrap gap, safety-favoring direction" posture used throughout this
-- session (e.g. PRJ-01's own unmodeled `dimensions` field) — so this SoD
-- is NOT enforced in this v1; CertifyProfitabilitySnapshot does not
-- refuse self-certification. Stated honestly rather than inventing a
-- fake authorship concept to gate against.
CREATE TABLE project_profitability_projections (
    projection_id                UUID PRIMARY KEY,
    tenant_id                       VARCHAR(255) NOT NULL,
    legal_entity_id                    VARCHAR(255) NOT NULL,
    project_id                            UUID NOT NULL REFERENCES projects(project_id),
    status                                    VARCHAR(20) NOT NULL, -- CURRENT|STALE|REBUILDING
    revenue                                      NUMERIC(18,2) NOT NULL,
    cost                                            NUMERIC(18,2) NOT NULL,
    margin                                            NUMERIC(18,2) NOT NULL,
    billed_amount                                        NUMERIC(18,2) NOT NULL,
    unbilled_amount                                        NUMERIC(18,2) NOT NULL,
    cost_watermark_at                                        TIMESTAMP WITH TIME ZONE NOT NULL, -- real MAX(created_at) over live project_cost_entries at refresh time
    revenue_run_id                                              UUID, -- the recognition run this projection's own revenue/billed figures were read from; NULL only when the project has no usable run yet
    revenue_watermark_at                                          TIMESTAMP WITH TIME ZONE, -- that run's own calculated_at; NULL under the same condition
    refreshed_at                                                    TIMESTAMP WITH TIME ZONE NOT NULL,
    refreshed_by_principal_id                                         VARCHAR(255) NOT NULL,
    created_at                                                          TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT chk_profitability_projection_status CHECK (status IN ('CURRENT', 'STALE', 'REBUILDING'))
);

-- One live projection per project — RefreshProfitabilityProjection is a
-- real upsert, never a second competing row.
CREATE UNIQUE INDEX idx_profitability_projections_project
    ON project_profitability_projections (tenant_id, project_id);

CREATE TABLE project_profitability_snapshots (
    snapshot_id                  UUID PRIMARY KEY,
    tenant_id                       VARCHAR(255) NOT NULL,
    legal_entity_id                    VARCHAR(255) NOT NULL,
    project_id                            UUID NOT NULL REFERENCES projects(project_id),
    status                                    VARCHAR(20) NOT NULL, -- DRAFT|RECONCILED|CERTIFIED (DRAFT is named by the spec's own state model but unreachable in this v1 — see doc comment above)
    revenue                                      NUMERIC(18,2) NOT NULL,
    cost                                            NUMERIC(18,2) NOT NULL,
    margin                                            NUMERIC(18,2) NOT NULL,
    billed_amount                                        NUMERIC(18,2) NOT NULL,
    unbilled_amount                                        NUMERIC(18,2) NOT NULL,
    cost_watermark_at                                        TIMESTAMP WITH TIME ZONE NOT NULL, -- frozen copy of the projection's own watermark at build time — this snapshot's own evidence
    revenue_run_id                                              UUID,
    revenue_watermark_at                                          TIMESTAMP WITH TIME ZONE,
    built_at                                                        TIMESTAMP WITH TIME ZONE NOT NULL,
    built_by_principal_id                                             VARCHAR(255) NOT NULL,
    certified_at                                                        TIMESTAMP WITH TIME ZONE,
    certified_by_principal_id                                             VARCHAR(255),

    CONSTRAINT chk_profitability_snapshot_status CHECK (status IN ('DRAFT', 'RECONCILED', 'CERTIFIED'))
);

-- Negative path #2's own structural backbone — once a snapshot row
-- exists, its own economic figures and watermarks are immutable; only
-- status/certification metadata may change afterward (RECONCILED ->
-- CERTIFIED).
CREATE OR REPLACE FUNCTION reject_profitability_snapshot_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.revenue IS DISTINCT FROM OLD.revenue OR
       NEW.cost IS DISTINCT FROM OLD.cost OR
       NEW.margin IS DISTINCT FROM OLD.margin OR
       NEW.billed_amount IS DISTINCT FROM OLD.billed_amount OR
       NEW.unbilled_amount IS DISTINCT FROM OLD.unbilled_amount OR
       NEW.cost_watermark_at IS DISTINCT FROM OLD.cost_watermark_at OR
       NEW.revenue_run_id IS DISTINCT FROM OLD.revenue_run_id OR
       NEW.revenue_watermark_at IS DISTINCT FROM OLD.revenue_watermark_at
    THEN
        RAISE EXCEPTION 'project_profitability_snapshots: economic figures and watermarks are immutable once built — build a new snapshot, never UPDATE this one';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_profitability_snapshot_mutation
    BEFORE UPDATE ON project_profitability_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_profitability_snapshot_mutation();

ALTER TABLE project_profitability_projections ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_profitability_projections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_profitability_projections
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE project_profitability_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_profitability_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_profitability_snapshots
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_profitability_snapshots_project ON project_profitability_snapshots (tenant_id, project_id);
