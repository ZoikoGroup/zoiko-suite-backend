-- =============================================================================
-- Migration: 0043_canonical_source_to_report_lineage.sql
-- Description: Canonical Schema for Governance, Evidence & Source-to-Report Lineage
-- Compliance: ZS-DATA-001 §23, §24, §29, §30 (Auditability & Acceptance Invariants)
-- Tenancy: RLS enabled + forced on all tables; fail-closed zoiko_backend role
-- =============================================================================

CREATE SCHEMA IF NOT EXISTS governance_lineage;

-- -----------------------------------------------------------------------------
-- 1. Lineage Edges (ZS-DATA-001 §23, §30 Scenario A12)
-- Directed acyclic graph recording source-to-report provenance
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_lineage.lineage_edges (
    edge_id                 UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    source_entity_type      VARCHAR(64) NOT NULL,
    source_entity_id        UUID NOT NULL,
    target_entity_type      VARCHAR(64) NOT NULL,
    target_entity_id        UUID NOT NULL,
    relationship_type       VARCHAR(64) NOT NULL,
    transformation_rule     VARCHAR(128) NOT NULL,
    system_recorded_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    valid_from              TIMESTAMPTZ NOT NULL,
    valid_to                TIMESTAMPTZ NULL,
    CONSTRAINT uq_lineage_edges_pair UNIQUE (tenant_id, source_entity_id, target_entity_id, relationship_type)
);

CREATE INDEX IF NOT EXISTS idx_lineage_edges_tenant_forward
    ON governance_lineage.lineage_edges (tenant_id, source_entity_type, source_entity_id);

CREATE INDEX IF NOT EXISTS idx_lineage_edges_tenant_backward
    ON governance_lineage.lineage_edges (tenant_id, target_entity_type, target_entity_id);

CREATE INDEX IF NOT EXISTS idx_lineage_edges_tenant_recorded
    ON governance_lineage.lineage_edges (tenant_id, system_recorded_at);

-- -----------------------------------------------------------------------------
-- 2. Evidence Events (ZS-DATA-001 §24, §30 Scenario A10, A11)
-- Cryptographically hashed immutable evidence records
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_lineage.evidence_events (
    evidence_id             UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    event_id                UUID NOT NULL,
    event_type              VARCHAR(128) NOT NULL,
    source_service          VARCHAR(128) NOT NULL,
    occurred_at             TIMESTAMPTZ NOT NULL,
    recorded_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    actor_type              VARCHAR(64) NOT NULL,
    actor_id                VARCHAR(128) NOT NULL,
    payload_digest_sha256   CHAR(64) NOT NULL,
    payload_format          VARCHAR(32) NOT NULL DEFAULT 'application/json',
    correlation_id          VARCHAR(128) NULL,
    causation_id            VARCHAR(128) NULL,
    CONSTRAINT uq_evidence_events_event_id UNIQUE (tenant_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_evidence_events_tenant_occurred
    ON governance_lineage.evidence_events (tenant_id, event_type, occurred_at);

CREATE INDEX IF NOT EXISTS idx_evidence_events_tenant_correlation
    ON governance_lineage.evidence_events (tenant_id, correlation_id)
    WHERE correlation_id IS NOT NULL;

-- -----------------------------------------------------------------------------
-- 3. Report Runs (ZS-DATA-001 §29, §30 Scenario A12)
-- Execution snapshot tying financial and regulatory statements to lineage hash
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS governance_lineage.report_runs (
    report_run_id           UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    report_definition_code  VARCHAR(64) NOT NULL,
    accounting_book_id      UUID NOT NULL,
    as_of_valid_time        TIMESTAMPTZ NOT NULL,
    as_of_system_time       TIMESTAMPTZ NOT NULL,
    lineage_snapshot_hash   CHAR(64) NOT NULL,
    status                  VARCHAR(32) NOT NULL,
    generated_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT chk_report_runs_status CHECK (status IN ('PENDING', 'COMPLETED', 'FAILED', 'ARCHIVED'))
);

CREATE INDEX IF NOT EXISTS idx_report_runs_tenant_def_asof
    ON governance_lineage.report_runs (tenant_id, report_definition_code, as_of_valid_time);

CREATE INDEX IF NOT EXISTS idx_report_runs_tenant_book
    ON governance_lineage.report_runs (tenant_id, accounting_book_id, generated_at);

-- -----------------------------------------------------------------------------
-- Row-Level Security Policies (Multi-Tenant Fail-Closed)
-- -----------------------------------------------------------------------------

ALTER TABLE governance_lineage.lineage_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_lineage.lineage_edges FORCE ROW LEVEL SECURITY;

CREATE POLICY lineage_edges_tenant_isolation ON governance_lineage.lineage_edges
    FOR ALL
    TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_lineage.evidence_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_lineage.evidence_events FORCE ROW LEVEL SECURITY;

CREATE POLICY evidence_events_tenant_isolation ON governance_lineage.evidence_events
    FOR ALL
    TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

ALTER TABLE governance_lineage.report_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_lineage.report_runs FORCE ROW LEVEL SECURITY;

CREATE POLICY report_runs_tenant_isolation ON governance_lineage.report_runs
    FOR ALL
    TO zoiko_backend
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

-- Grant schema access to backend role
GRANT USAGE ON SCHEMA governance_lineage TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA governance_lineage TO zoiko_backend;
