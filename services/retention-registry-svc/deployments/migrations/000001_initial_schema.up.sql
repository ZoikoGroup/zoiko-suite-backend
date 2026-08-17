-- 000001_initial_schema.up.sql
-- Retention Registry Service — initial schema
--
-- docs/original_doc/zoiko_suite_doc7.txt §J1: "By record type, tenant
-- contract, jurisdiction, legal/regulatory need, source rights, privacy
-- obligations and legal hold... Version retention_policy; every purge
-- checks legal hold, record class, minimum/maximum rules and
-- source-system ownership before deletion/redaction."
--
-- §J2: "No automatic destructive deletion of governed records... Separate
-- access state, commercial subscription state and data-retention state."
-- — three deliberately independent dimensions, same doctrine this
-- codebase applies everywhere (capability existence vs. market vs.
-- release state; design status vs. operating effectiveness).
--
-- §J3: "It blocks deletion/redaction actions covered by the hold until
-- authorized release... legal_hold specifies scope, custodians/objects,
-- authority, start, status and release approval. Hold checks occur in
-- deletion/export/migration paths."
--
-- Owns:
--   retention_policies — versioned rules for how long a record class may
--     be kept. Immutable once created (same doctrine as policy_versions):
--     a changed policy is a new row, never an UPDATE.
--   legal_holds — an override that freezes deletion/export/migration for
--     a scope regardless of what retention_policies says, until released.
--
-- Neither table deletes anything itself. This service answers exactly one
-- question for every OTHER service that owns deletable data: "is it safe
-- to delete/export/migrate this right now?" — see the resolve endpoint.

-- ── retention_policies ───────────────────────────────────────────────────────

CREATE TABLE retention_policies (
    retention_policy_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Free-text record-class identifier — DATA ONLY, no registry backs it
    -- yet (same doctrine as kill-switch-registry-svc's domain column and
    -- policy-svc's policy_type). E.g. FINANCIAL_LEDGER, OBLIGATION_EVIDENCE,
    -- COMMERCIAL_SUBSCRIPTION_HISTORY.
    record_class               VARCHAR(128) NOT NULL,

    -- NULL = applies regardless of jurisdiction.
    jurisdiction_code           VARCHAR(64),
    -- NULL = applies platform-wide, not one tenant.
    tenant_id                     UUID,

    -- §J1's "minimum/maximum rules" — NULL max_retention_days means no
    -- upper bound (keep indefinitely is a real, explicit choice, not a
    -- missing value).
    min_retention_days             INTEGER NOT NULL,
    max_retention_days               INTEGER,

    legal_regulatory_basis             TEXT NOT NULL,
    source_rights_basis                   TEXT,
    privacy_basis                            TEXT,

    -- DRAFT | ACTIVE | SUPERSEDED | RETIRED — VARCHAR, not enum. Same
    -- state machine shape as policy_versions.version_status.
    policy_status                              VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',

    effective_from                                TIMESTAMPTZ NOT NULL,
    effective_to                                     TIMESTAMPTZ,

    created_at                                          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                                TEXT        NOT NULL
);

CREATE INDEX idx_retention_policies_scope
    ON retention_policies (record_class, jurisdiction_code, tenant_id, effective_from DESC);

-- ── legal_holds ───────────────────────────────────────────────────────────────

CREATE TABLE legal_holds (
    legal_hold_id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    scope_description         TEXT        NOT NULL,
    -- Free-text list of custodians/object references — §J3's
    -- "custodians/objects". JSONB so a hold can name several at once.
    custodians_objects           JSONB       NOT NULL DEFAULT '[]',
    -- Who ordered this hold (e.g. "Legal Counsel — Case #4821", a court
    -- order reference) — §J3's "authority".
    authority                       TEXT        NOT NULL,

    -- NULL = not scoped to one record class / tenant / entity — the same
    -- nullable-scope doctrine as kill_switch_events, broadest first.
    record_class                      VARCHAR(128),
    tenant_id                            UUID,
    entity_ref                              VARCHAR(255),

    -- ACTIVE | RELEASED — VARCHAR, not enum.
    hold_status                                VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',

    started_at                                    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at                                      TIMESTAMPTZ,
    released_by_principal_id                            TEXT,
    -- §J3's "release approval" — required whenever hold_status transitions
    -- to RELEASED; nullable while still ACTIVE.
    release_approved_by_principal_id                       TEXT,

    created_at                                                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                                      TEXT        NOT NULL
);

CREATE INDEX idx_legal_holds_scope
    ON legal_holds (record_class, tenant_id, entity_ref)
    WHERE hold_status = 'ACTIVE';
