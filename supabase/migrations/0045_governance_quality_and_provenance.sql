-- =============================================================================
-- Migration: 0045_governance_quality_and_provenance.sql
-- Description: Canonical Schema for Data Quality (Rules, Runs, Results, Issues)
--              and Extended Lineage/Provenance (Lineage Nodes, Provenance Assertions).
-- Compliance: ZS-DATA-GOV-001 §6, §7, §8, §9, §28 (Quality & Lineage Engine)
-- Controls: DG-011, DG-012, DG-013, DG-014, DG-015, DG-016, DG-017, DG-018, DG-019,
--           DG-020, DG-021, DG-022, DG-023, DG-024, DG-025, DG-026, DG-027
-- Invariants: GOV-06, GOV-07, GOV-08, GOV-09, GOV-10, NP-04, NP-05, NP-06, NP-07, NP-08
-- Tenancy: RLS enabled + forced on all tables; fail-closed zoiko_backend role
-- =============================================================================

CREATE SCHEMA IF NOT EXISTS governance_quality;

-- -----------------------------------------------------------------------------
-- 1. Data Quality Rules (ZS-DATA-GOV-001 §6.2, DG-011, NP-04)
-- Versioned, immutable quality assertions
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_quality.dq_rules (
    rule_id                 UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    asset_id                UUID NOT NULL,
    rule_code               VARCHAR(64) NOT NULL,
    version                 INTEGER NOT NULL DEFAULT 1,
    dimension               VARCHAR(32) NOT NULL,
    assertion_type          VARCHAR(64) NOT NULL,
    parameters              TEXT NOT NULL DEFAULT '{}',
    threshold_pct           NUMERIC(5,2) NOT NULL DEFAULT 100.00,
    severity                VARCHAR(32) NOT NULL,
    is_active               BOOLEAN NOT NULL DEFAULT TRUE,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_by              VARCHAR(128) NOT NULL,
    CONSTRAINT uq_dq_rules_asset_version UNIQUE (tenant_id, asset_id, rule_code, version),
    CONSTRAINT chk_dq_rules_dimension CHECK (dimension IN ('COMPLETENESS', 'VALIDITY', 'ACCURACY', 'CONSISTENCY', 'TIMELINESS', 'UNIQUENESS')),
    CONSTRAINT chk_dq_rules_severity CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    CONSTRAINT chk_dq_rules_threshold CHECK (threshold_pct >= 0.00 AND threshold_pct <= 100.00)
);

CREATE INDEX IF NOT EXISTS idx_dq_rules_lookup
    ON governance_quality.dq_rules (tenant_id, asset_id, is_active);

-- -----------------------------------------------------------------------------
-- 2. Data Quality Runs (ZS-DATA-GOV-001 §6.2, DG-012, NP-05)
-- Execution snapshot bound to reproducible watermark/population
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_quality.dq_runs (
    run_id                  UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    rule_id                 UUID NOT NULL REFERENCES governance_quality.dq_rules(rule_id),
    rule_version            INTEGER NOT NULL,
    population_identity     VARCHAR(128) NOT NULL,
    evaluated_count         BIGINT NOT NULL DEFAULT 0,
    passed_count            BIGINT NOT NULL DEFAULT 0,
    failed_count            BIGINT NOT NULL DEFAULT 0,
    pass_rate               NUMERIC(5,2) NOT NULL DEFAULT 0.00,
    status                  VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    run_by                  VARCHAR(128) NOT NULL,
    started_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at            TIMESTAMPTZ NULL,
    CONSTRAINT chk_dq_runs_status CHECK (status IN ('PENDING', 'COMPLETED', 'FAILED')),
    CONSTRAINT chk_dq_runs_counts CHECK (evaluated_count >= 0 AND passed_count >= 0 AND failed_count >= 0)
);

CREATE INDEX IF NOT EXISTS idx_dq_runs_tenant_rule
    ON governance_quality.dq_runs (tenant_id, rule_id, started_at DESC);

-- -----------------------------------------------------------------------------
-- 3. Data Quality Results (ZS-DATA-GOV-001 §6.2)
-- Granular metrics and violation records from an execution run
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_quality.dq_results (
    result_id               UUID PRIMARY KEY,
    run_id                  UUID NOT NULL REFERENCES governance_quality.dq_runs(run_id),
    tenant_id               UUID NOT NULL,
    rule_id                 UUID NOT NULL REFERENCES governance_quality.dq_rules(rule_id),
    passed                  BOOLEAN NOT NULL,
    metric_values           TEXT NOT NULL DEFAULT '{}',
    sample_violations       TEXT NULL,
    evaluated_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS idx_dq_results_run
    ON governance_quality.dq_results (tenant_id, run_id);

-- -----------------------------------------------------------------------------
-- 4. Data Quality Issues & Remediation (ZS-DATA-GOV-001 §7, DG-018..DG-020, NP-06)
-- Governed quality exception requiring source-domain remediation + reperformance
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_quality.dq_issues (
    issue_id                UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    run_id                  UUID NOT NULL REFERENCES governance_quality.dq_runs(run_id),
    rule_id                 UUID NOT NULL REFERENCES governance_quality.dq_rules(rule_id),
    asset_id                UUID NOT NULL,
    severity                VARCHAR(32) NOT NULL,
    status                  VARCHAR(32) NOT NULL DEFAULT 'OPEN',
    assigned_to             VARCHAR(128) NULL,
    sla_due_at              TIMESTAMPTZ NOT NULL,
    authoritative_remediation_ref VARCHAR(255) NULL,
    reperformance_run_id    UUID NULL REFERENCES governance_quality.dq_runs(run_id),
    opened_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at             TIMESTAMPTZ NULL,
    CONSTRAINT chk_dq_issues_severity CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    CONSTRAINT chk_dq_issues_status CHECK (status IN ('OPEN', 'ASSIGNED', 'REMEDIATED', 'CLOSED'))
);

CREATE INDEX IF NOT EXISTS idx_dq_issues_tenant_status
    ON governance_quality.dq_issues (tenant_id, status, severity);

-- -----------------------------------------------------------------------------
-- 5. Extended Lineage Nodes (ZS-DATA-GOV-001 §8, §28)
-- Categorized nodes in the provenance graph (Entity, Activity, Agent, ReportField)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_lineage.lineage_nodes (
    node_id                 UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    node_type               VARCHAR(32) NOT NULL,
    node_code               VARCHAR(128) NOT NULL,
    node_name               VARCHAR(255) NOT NULL,
    domain_id               UUID NOT NULL,
    metadata                TEXT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_lineage_nodes_code UNIQUE (tenant_id, node_code),
    CONSTRAINT chk_lineage_nodes_type CHECK (node_type IN ('ENTITY', 'ACTIVITY', 'AGENT', 'REPORT_FIELD'))
);

CREATE INDEX IF NOT EXISTS idx_lineage_nodes_tenant_type
    ON governance_lineage.lineage_nodes (tenant_id, node_type);

-- -----------------------------------------------------------------------------
-- 6. Provenance Assertions (ZS-DATA-GOV-001 §8, DG-021..DG-023, NP-08)
-- Immutable source, activity, rule version, and agent attribution
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_lineage.provenance_assertions (
    assertion_id            UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    subject_entity_type     VARCHAR(64) NOT NULL,
    subject_entity_id       UUID NOT NULL,
    source_activity_id      UUID NOT NULL,
    agent_principal_id      VARCHAR(128) NOT NULL,
    transformation_rule_version VARCHAR(64) NOT NULL,
    source_hashes           TEXT NOT NULL,
    system_recorded_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    valid_from              TIMESTAMPTZ NOT NULL,
    valid_to                TIMESTAMPTZ NULL,
    CONSTRAINT chk_provenance_effective_dates CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE INDEX IF NOT EXISTS idx_provenance_assertions_subject
    ON governance_lineage.provenance_assertions (tenant_id, subject_entity_type, subject_entity_id, valid_from);

-- -----------------------------------------------------------------------------
-- Row-Level Security Policies (Multi-Tenant Fail-Closed)
-- -----------------------------------------------------------------------------

ALTER TABLE governance_quality.dq_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_quality.dq_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_rules_tenant_isolation ON governance_quality.dq_rules
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_quality.dq_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_quality.dq_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_runs_tenant_isolation ON governance_quality.dq_runs
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_quality.dq_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_quality.dq_results FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_results_tenant_isolation ON governance_quality.dq_results
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_quality.dq_issues ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_quality.dq_issues FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_issues_tenant_isolation ON governance_quality.dq_issues
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_lineage.lineage_nodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_lineage.lineage_nodes FORCE ROW LEVEL SECURITY;
CREATE POLICY lineage_nodes_tenant_isolation ON governance_lineage.lineage_nodes
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_lineage.provenance_assertions ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_lineage.provenance_assertions FORCE ROW LEVEL SECURITY;
CREATE POLICY provenance_assertions_tenant_isolation ON governance_lineage.provenance_assertions
    FOR ALL TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

-- Grant schema access to backend role
GRANT USAGE ON SCHEMA governance_quality TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_quality TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_lineage TO zoiko_backend;
