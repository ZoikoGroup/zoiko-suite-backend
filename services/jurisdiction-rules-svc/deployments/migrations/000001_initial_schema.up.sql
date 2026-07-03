-- 000001_initial_schema.up.sql
-- Jurisdiction Rules Service — initial schema
--
-- Owns:
--   jurisdictions        — authoritative registry of jurisdiction identities
--   jurisdiction_rules   — effective-dated, versioned applicability rule records
--
-- Design decisions:
--   - All type/status/domain fields are VARCHAR — no enums. New values added
--     via data migration only; zero code changes required (OQ-3).
--   - jurisdiction_rules.rule_payload is JSONB carrying applicability
--     METADATA only — NOT computation values (thresholds, rates). Those
--     belong to Tax/Payroll services (OQ-1, Model B).
--   - legal_drift_state transitions are append-only via
--     jurisdiction_rule_drift_events. The column on jurisdiction_rules
--     reflects current state; full history is in the events table (OQ-4).
--   - No soft-delete. Deactivation uses active_flag + effective_to.
--   - No RLS on jurisdictions/jurisdiction_rules — these are platform-wide
--     reference data, not per-tenant data. No tenant_id column.

-- ── jurisdictions ────────────────────────────────────────────────────────────

CREATE TABLE jurisdictions (
    jurisdiction_id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Human-readable code — data only, never used as switch/case (doctrine)
    jurisdiction_code       VARCHAR(32) NOT NULL,

    jurisdiction_name       TEXT        NOT NULL,

    -- VARCHAR — extensible via data migration. E.g. COUNTRY, STATE_PROVINCE,
    -- TAX_AUTHORITY, LABOR_LAW_BOUNDARY, FILING_AUTHORITY,
    -- DATA_RESIDENCY_BOUNDARY. Adding new types = data only, no code change.
    jurisdiction_type       VARCHAR(64) NOT NULL,

    -- Self-referential hierarchy. NULL = root jurisdiction.
    -- Supports arbitrary depth: country → state → tax authority.
    parent_jurisdiction_id  UUID        REFERENCES jurisdictions(jurisdiction_id),

    -- FEDERAL, STATE, MUNICIPAL, SUPRANATIONAL — data driven.
    authority_type          VARCHAR(64) NOT NULL,

    -- Effective dating: when this jurisdiction record became valid.
    effective_from          TIMESTAMPTZ NOT NULL,
    -- NULL = currently active. End-dating, not deletion.
    effective_to            TIMESTAMPTZ,

    -- Operational deactivation. Not a soft-delete — record is preserved.
    active_flag             BOOLEAN     NOT NULL DEFAULT TRUE,

    -- Audit
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT        NOT NULL,
    schema_version          VARCHAR(16) NOT NULL DEFAULT '1.0'
);

-- Idempotent creation key: same code + type + parent = same jurisdiction.
-- NULL parent is included via COALESCE so the constraint works correctly.
CREATE UNIQUE INDEX idx_jurisdictions_code_type_parent_unique
    ON jurisdictions (jurisdiction_code, jurisdiction_type, COALESCE(parent_jurisdiction_id, '00000000-0000-0000-0000-000000000000'::UUID));

-- Lookup by code (callers resolving by human code)
CREATE INDEX idx_jurisdictions_code      ON jurisdictions (jurisdiction_code);
-- Hierarchy traversal — find children of a given parent
CREATE INDEX idx_jurisdictions_parent    ON jurisdictions (parent_jurisdiction_id)
    WHERE parent_jurisdiction_id IS NOT NULL;
-- Active filter
CREATE INDEX idx_jurisdictions_active    ON jurisdictions (active_flag)
    WHERE active_flag = TRUE;

-- ── jurisdiction_rules ───────────────────────────────────────────────────────

CREATE TABLE jurisdiction_rules (
    jurisdiction_rule_id    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id         UUID         NOT NULL REFERENCES jurisdictions(jurisdiction_id),

    -- Domain tag — data driven. E.g. PAYROLL, TAX, EMPLOYMENT, FILING,
    -- RETENTION, BENEFITS. Never used as a code switch/case.
    rule_domain             VARCHAR(64)  NOT NULL,

    -- Stable machine-readable identifier for this rule type within a domain.
    rule_code               VARCHAR(128) NOT NULL,
    rule_name               TEXT         NOT NULL,

    -- Effective dating — point-in-time queries use:
    --   effective_from <= $at AND (effective_to IS NULL OR effective_to > $at)
    effective_from          TIMESTAMPTZ  NOT NULL,
    effective_to            TIMESTAMPTZ,

    -- Applicability metadata ONLY (OQ-1 Model B).
    -- Structure is domain-specific; schema governed by rule_domain + schema_version.
    -- Does NOT contain computation values (thresholds, rates, bands).
    -- Example: {"applies_to_entity_types": ["COMPANY","BRANCH"],
    --            "filing_frequency": "MONTHLY",
    --            "authority_code": "HMRC"}
    rule_payload            JSONB        NOT NULL DEFAULT '{}',

    -- Citation: legislative act, regulation, statutory instrument.
    source_reference        TEXT,

    -- ACTIVE | SUPERSEDED | DRAFT | RETIRED — VARCHAR, not enum.
    rule_status             VARCHAR(32)  NOT NULL DEFAULT 'DRAFT',

    -- Reference to external regulatory data feed that produced this rule.
    external_feed_reference TEXT,

    -- Current drift state. History is in jurisdiction_rule_drift_events (OQ-4).
    -- CURRENT | DRIFTED | UNDER_REVIEW
    legal_drift_state       VARCHAR(32)  NOT NULL DEFAULT 'CURRENT',

    -- Audit
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    schema_version          VARCHAR(16)  NOT NULL DEFAULT '1.0'
);

-- Primary lookup: effective rules for a jurisdiction + domain at a point in time
CREATE INDEX idx_jrules_jurisdiction_domain
    ON jurisdiction_rules (jurisdiction_id, rule_domain);

-- Point-in-time query support
CREATE INDEX idx_jrules_effective
    ON jurisdiction_rules (jurisdiction_id, rule_domain, effective_from, effective_to);

-- Rule status filter
CREATE INDEX idx_jrules_status
    ON jurisdiction_rules (rule_status)
    WHERE rule_status = 'ACTIVE';

-- Drift monitoring
CREATE INDEX idx_jrules_drift
    ON jurisdiction_rules (legal_drift_state)
    WHERE legal_drift_state != 'CURRENT';

-- ── jurisdiction_rule_drift_events ───────────────────────────────────────────
-- Append-only history of legal_drift_state transitions (OQ-4).
-- The jurisdiction_rules.legal_drift_state column reflects current state;
-- this table preserves the full transition history without overwriting.

CREATE TABLE jurisdiction_rule_drift_events (
    drift_event_id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_rule_id    UUID        NOT NULL REFERENCES jurisdiction_rules(jurisdiction_rule_id),
    from_state              VARCHAR(32) NOT NULL,
    to_state                VARCHAR(32) NOT NULL,
    reason                  TEXT,
    effective_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    recorded_by_principal_id TEXT       NOT NULL,
    correlation_id          TEXT,
    schema_version          VARCHAR(16) NOT NULL DEFAULT '1.0'
);

-- History lookup per rule
CREATE INDEX idx_drift_events_rule
    ON jurisdiction_rule_drift_events (jurisdiction_rule_id, effective_at DESC);
