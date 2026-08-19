-- 0002_jurisdiction_rules_svc.sql
-- jurisdiction-rules-svc → schema `jurisdiction_rules`
--
-- Squashed end state of the service's four compose-era migrations:
--   000001_initial_schema, 000002_add_audit_columns,
--   000003_add_data_classification, 000004_add_rule_code_index
--
-- This is a NEW database, so the history is squashed rather than replayed —
-- the four files exist to get an already-running volume to this shape, and
-- nothing here needs that. The compose migrations remain the source of truth
-- for the docker estate until it is retired.
--
-- Three tables: jurisdictions, jurisdiction_rules, jurisdiction_rule_drift_events.
--
-- ── Tenancy ──────────────────────────────────────────────────────────────────
-- This service owns PLATFORM-WIDE REFERENCE DATA. There is no tenant_id column
-- on any of its tables, and that is deliberate — 000001 says so explicitly. So
-- the tenant policies used by the other nineteen services do not apply here.
--
-- RLS is still enabled and forced, because the read/write asymmetry matters:
-- these tables are readable by any authenticated principal and writable only by
-- the backend. Without RLS, exposing this schema through PostgREST would let
-- any logged-in console user rewrite the jurisdiction registry every other
-- service resolves against.

CREATE SCHEMA IF NOT EXISTS jurisdiction_rules;

COMMENT ON SCHEMA jurisdiction_rules IS
    'jurisdiction-rules-svc. Platform-wide reference data: jurisdiction identities and effective-dated applicability rules. No tenant dimension by design.';

GRANT USAGE ON SCHEMA jurisdiction_rules TO zoiko_backend, authenticated;

-- ── jurisdictions ────────────────────────────────────────────────────────────

CREATE TABLE jurisdiction_rules.jurisdictions (
    jurisdiction_id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Human-readable code — data only, never used as switch/case (doctrine).
    jurisdiction_code       VARCHAR(32) NOT NULL,
    jurisdiction_name       TEXT        NOT NULL,

    -- VARCHAR, not an enum, so new values are a data migration and not a code
    -- change: COUNTRY, STATE_PROVINCE, TAX_AUTHORITY, LABOR_LAW_BOUNDARY,
    -- FILING_AUTHORITY, DATA_RESIDENCY_BOUNDARY.
    jurisdiction_type       VARCHAR(64) NOT NULL,

    -- Self-referential hierarchy of arbitrary depth (country → state → tax
    -- authority). NULL = root.
    parent_jurisdiction_id  UUID        REFERENCES jurisdiction_rules.jurisdictions(jurisdiction_id),

    -- FEDERAL, STATE, MUNICIPAL, SUPRANATIONAL — data driven.
    authority_type          VARCHAR(64) NOT NULL,

    -- Effective dating. NULL effective_to = currently valid. End-dating, never
    -- deletion.
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ,

    -- Operational deactivation, not a soft delete: the row is preserved and
    -- stays visible to GET /v1/jurisdictions so a register can still explain a
    -- historical record. It does stop resolving — the single-jurisdiction
    -- lookup is ACTIVE-ONLY and 404s once this is false, which is deliberate
    -- and consequential for records ALREADY bound to it.
    active_flag             BOOLEAN     NOT NULL DEFAULT TRUE,

    -- Audit
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT        NOT NULL DEFAULT app.current_principal_id(),
    updated_at              TIMESTAMPTZ,
    updated_by_principal_id TEXT,
    schema_version          VARCHAR(16) NOT NULL DEFAULT '1.0',

    -- PUBLIC tier: names, region codes, country codes.
    data_classification     VARCHAR(32) NOT NULL DEFAULT 'PUBLIC'
);

-- Idempotent creation key: same code + type + parent = same jurisdiction.
-- COALESCE so a NULL parent participates rather than making every root row
-- distinct from every other.
CREATE UNIQUE INDEX idx_jurisdictions_code_type_parent_unique
    ON jurisdiction_rules.jurisdictions (
        jurisdiction_code,
        jurisdiction_type,
        COALESCE(parent_jurisdiction_id, '00000000-0000-0000-0000-000000000000'::UUID)
    );

CREATE INDEX idx_jurisdictions_code   ON jurisdiction_rules.jurisdictions (jurisdiction_code);
CREATE INDEX idx_jurisdictions_parent ON jurisdiction_rules.jurisdictions (parent_jurisdiction_id)
    WHERE parent_jurisdiction_id IS NOT NULL;
CREATE INDEX idx_jurisdictions_active ON jurisdiction_rules.jurisdictions (active_flag)
    WHERE active_flag = TRUE;

-- ── jurisdiction_rules ───────────────────────────────────────────────────────

CREATE TABLE jurisdiction_rules.jurisdiction_rules (
    jurisdiction_rule_id    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id         UUID         NOT NULL
                                         REFERENCES jurisdiction_rules.jurisdictions(jurisdiction_id),

    -- Domain tag — data driven: PAYROLL, TAX, EMPLOYMENT, FILING, RETENTION,
    -- BENEFITS. Never a code switch/case.
    rule_domain             VARCHAR(64)  NOT NULL,

    rule_code               VARCHAR(128) NOT NULL,
    rule_name               TEXT         NOT NULL,

    -- Point-in-time queries use the half-open interval:
    --   effective_from <= $at AND (effective_to IS NULL OR effective_to > $at)
    effective_from          TIMESTAMPTZ  NOT NULL,
    effective_to            TIMESTAMPTZ,

    -- Applicability METADATA only — never computation values (thresholds,
    -- rates, bands), which belong to the Tax and Payroll services.
    rule_payload            JSONB        NOT NULL DEFAULT '{}',

    -- Citation: legislative act, regulation, statutory instrument.
    source_reference        TEXT,

    -- ACTIVE | SUPERSEDED | DRAFT | RETIRED
    rule_status             VARCHAR(32)  NOT NULL DEFAULT 'DRAFT',

    external_feed_reference TEXT,

    -- Current state only. Full transition history is append-only in
    -- jurisdiction_rule_drift_events. CURRENT | DRIFTED | UNDER_REVIEW
    legal_drift_state       VARCHAR(32)  NOT NULL DEFAULT 'CURRENT',

    -- Audit
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL DEFAULT app.current_principal_id(),
    updated_at              TIMESTAMPTZ,
    updated_by_principal_id TEXT,
    schema_version          VARCHAR(16)  NOT NULL DEFAULT '1.0',

    -- INTERNAL tier: rule domain settings, legislative metadata.
    data_classification     VARCHAR(32)  NOT NULL DEFAULT 'INTERNAL'
);

-- Dedup key for idempotent rule creation.
CREATE UNIQUE INDEX idx_jrules_jurisdiction_code_effective_from_unique
    ON jurisdiction_rules.jurisdiction_rules (jurisdiction_id, rule_code, effective_from);

-- Primary lookup: rules for a jurisdiction + domain.
CREATE INDEX idx_jrules_jurisdiction_domain
    ON jurisdiction_rules.jurisdiction_rules (jurisdiction_id, rule_domain);

-- Point-in-time resolution.
CREATE INDEX idx_jrules_effective
    ON jurisdiction_rules.jurisdiction_rules (jurisdiction_id, rule_domain, effective_from, effective_to);

CREATE INDEX idx_jrules_status
    ON jurisdiction_rules.jurisdiction_rules (rule_status)
    WHERE rule_status = 'ACTIVE';

CREATE INDEX idx_jrules_drift
    ON jurisdiction_rules.jurisdiction_rules (legal_drift_state)
    WHERE legal_drift_state != 'CURRENT';

-- Serves hasOverlappingRule (filters on jurisdiction_id + rule_domain +
-- rule_code before comparing periods, on every create and every transition
-- into a live status) and FindRulePack's DISTINCT ON across an ancestor chain.
-- idx_jrules_effective cannot serve either — rule_code is not in it.
CREATE INDEX idx_jrules_domain_code
    ON jurisdiction_rules.jurisdiction_rules (jurisdiction_id, rule_domain, rule_code, effective_from);

-- ── jurisdiction_rule_drift_events ───────────────────────────────────────────
-- Append-only history of legal_drift_state transitions. The column on
-- jurisdiction_rules holds current state; this preserves the full chain without
-- overwriting. "Append-only" is enforced below by granting INSERT and SELECT
-- and no policy for UPDATE or DELETE, rather than by convention.

CREATE TABLE jurisdiction_rules.jurisdiction_rule_drift_events (
    drift_event_id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_rule_id     UUID        NOT NULL
                                         REFERENCES jurisdiction_rules.jurisdiction_rules(jurisdiction_rule_id),
    from_state               VARCHAR(32) NOT NULL,
    to_state                 VARCHAR(32) NOT NULL,
    reason                   TEXT,
    effective_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    recorded_by_principal_id TEXT        NOT NULL DEFAULT app.current_principal_id(),
    correlation_id           TEXT,
    schema_version           VARCHAR(16) NOT NULL DEFAULT '1.0'
);

CREATE INDEX idx_drift_events_rule
    ON jurisdiction_rules.jurisdiction_rule_drift_events (jurisdiction_rule_id, effective_at DESC);

-- ── Row-level security ───────────────────────────────────────────────────────
-- ENABLE *and* FORCE on all three. FORCE is what makes the policies apply to
-- the role that owns the tables; without it the owner bypasses them and the
-- policies below would be decorative.

ALTER TABLE jurisdiction_rules.jurisdictions                    ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_rules.jurisdictions                    FORCE  ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_rules.jurisdiction_rules               ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_rules.jurisdiction_rules               FORCE  ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_rules.jurisdiction_rule_drift_events   ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_rules.jurisdiction_rule_drift_events   FORCE  ROW LEVEL SECURITY;

-- Read: any authenticated principal. This is reference data every other
-- service resolves against, and it carries no tenant dimension, so there is
-- nothing to partition reads by. `anon` is deliberately NOT granted — an
-- unauthenticated caller has no business enumerating the registry.
CREATE POLICY reference_data_read ON jurisdiction_rules.jurisdictions
    FOR SELECT TO authenticated, zoiko_backend USING (true);

CREATE POLICY reference_data_read ON jurisdiction_rules.jurisdiction_rules
    FOR SELECT TO authenticated, zoiko_backend USING (true);

CREATE POLICY reference_data_read ON jurisdiction_rules.jurisdiction_rule_drift_events
    FOR SELECT TO authenticated, zoiko_backend USING (true);

-- Write: backend only. Authorization for these routes is decided by
-- authorization-svc against the JURISDICTION_* / JURISDICTION_RULE_* actions;
-- the database's job is to ensure the write arrived through the service that
-- performs that check, not directly from a console session.
CREATE POLICY backend_write ON jurisdiction_rules.jurisdictions
    FOR INSERT TO zoiko_backend WITH CHECK (true);
CREATE POLICY backend_update ON jurisdiction_rules.jurisdictions
    FOR UPDATE TO zoiko_backend USING (true) WITH CHECK (true);

CREATE POLICY backend_write ON jurisdiction_rules.jurisdiction_rules
    FOR INSERT TO zoiko_backend WITH CHECK (true);
CREATE POLICY backend_update ON jurisdiction_rules.jurisdiction_rules
    FOR UPDATE TO zoiko_backend USING (true) WITH CHECK (true);

-- Drift events get INSERT and no UPDATE/DELETE policy, for anyone. An append-
-- only register that can be edited is not one.
CREATE POLICY backend_append ON jurisdiction_rules.jurisdiction_rule_drift_events
    FOR INSERT TO zoiko_backend WITH CHECK (true);

-- ── Grants ───────────────────────────────────────────────────────────────────
-- RLS narrows what a role may touch; it does not grant access in the first
-- place. Both are required.

GRANT SELECT ON ALL TABLES IN SCHEMA jurisdiction_rules TO authenticated;

GRANT SELECT, INSERT, UPDATE ON
    jurisdiction_rules.jurisdictions,
    jurisdiction_rules.jurisdiction_rules
    TO zoiko_backend;

-- No UPDATE, no DELETE — append-only, enforced by the grant as well as by the
-- absent policy.
GRANT SELECT, INSERT ON jurisdiction_rules.jurisdiction_rule_drift_events TO zoiko_backend;

-- DELETE is granted to nobody on any table in this schema. The service has no
-- delete route by design: deactivation is active_flag + effective_to.
