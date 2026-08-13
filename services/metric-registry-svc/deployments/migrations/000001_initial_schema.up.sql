-- 000001_initial_schema.up.sql
-- Metric Registry Service — initial schema
--
-- docs/original_doc/zoiko_suite_doc7.txt §27 REP-01: "Executive metrics
-- are defined, versioned, source-traceable and labeled so operational
-- intelligence is not misrepresented as financial/legal assurance." §33's
-- critical-drift table names the same risk directly: "Executive summaries
-- can hide stale/missing evidence or conflicting sources... Every metric/
-- status carries freshness, source, scope, coverage and exceptions;
-- drill-through is mandatory."
--
-- Owns:
--   report_metric_definitions — one row per (metric_code, version). A
--     "new version" of a metric is always a new row, never an UPDATE to
--     an existing one — same versioned-by-new-row doctrine as
--     control_test_definitions and policy_versions. At most one row per
--     metric_code is ACTIVE at a time (enforced by a partial unique
--     index, mirroring policy-svc's one-active-version-per-scope index).
--
-- This service defines what a metric IS and MEANS; it does not compute or
-- store metric VALUES — that stays in whatever reporting/BI layer reads
-- from this registry to label its own numbers correctly.

CREATE TABLE report_metric_definitions (
    metric_definition_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Stable identifier shared across every version of one metric.
    -- DATA ONLY, never a code switch/case.
    metric_code                VARCHAR(128) NOT NULL,

    metric_name                  TEXT        NOT NULL,

    -- Human-readable description of the calculation — doc7 REP-01's
    -- "defined" requirement. Free text by design: formulas vary too much
    -- to force into one structured shape without losing real detail.
    formula_description            TEXT        NOT NULL,

    -- doc7 REP-01's "source-traceable" requirement — which upstream
    -- systems/tables/services this metric is computed from. JSONB array
    -- of free-text source identifiers.
    data_sources                      JSONB       NOT NULL DEFAULT '[]',

    -- The function/person accountable for this metric's correctness —
    -- doc7's own SEGREGATION-OF-DUTIES doctrine requires an explicit
    -- owner for anything presented as executive intelligence.
    owner_principal_id                  TEXT        NOT NULL,

    -- doc7 REP-01's "labeled so operational intelligence is not
    -- misrepresented as financial/legal assurance" — every metric
    -- carries this disclaimer explicitly rather than relying on
    -- reporting-layer convention to add it.
    intelligence_disclaimer                TEXT        NOT NULL DEFAULT
        'Operational intelligence — not financial or legal assurance.',

    version                                   INTEGER     NOT NULL,

    -- DRAFT | ACTIVE | SUPERSEDED | RETIRED — VARCHAR, not enum. Same
    -- state machine shape as policy_versions.version_status.
    definition_status                            VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',

    effective_from                                  TIMESTAMPTZ NOT NULL,

    created_at                                         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                               TEXT        NOT NULL
);

-- Idempotent creation key: one row per metric_code+version.
CREATE UNIQUE INDEX idx_report_metric_definitions_version_unique
    ON report_metric_definitions (metric_code, version);

-- Enforce at most one ACTIVE version per metric_code at any time —
-- publishing a new version must supersede the prior ACTIVE row in the
-- same transaction, same doctrine as policy-svc's
-- idx_policy_versions_one_active_per_scope.
CREATE UNIQUE INDEX idx_report_metric_definitions_one_active
    ON report_metric_definitions (metric_code)
    WHERE definition_status = 'ACTIVE';

CREATE INDEX idx_report_metric_definitions_history
    ON report_metric_definitions (metric_code, version DESC);
