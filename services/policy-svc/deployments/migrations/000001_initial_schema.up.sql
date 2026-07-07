-- 000001_initial_schema.up.sql
-- Policy Service — initial schema
--
-- Owns:
--   policies         — authoritative named containers for policy definitions
--   policy_versions  — effective-dated, state-machined rule content per policy
--
-- Design decisions (mirrors jurisdiction-rules-svc's Jurisdiction/
-- JurisdictionRule shape — Policy <-> Jurisdiction, PolicyVersion <->
-- JurisdictionRule):
--   - policy_type and version_status are VARCHAR — no enums. New policy_type
--     values are added via data only, never a code change.
--   - policy_versions.rule_payload is JSONB; shape depends on policy_type.
--     Only APPROVAL_THRESHOLD has real evaluation logic in v1 (see handler).
--   - No soft-delete, no UPDATE/DELETE on either table. A change is always
--     either a new DRAFT version or a version_status transition.
--   - tenant_id / legal_entity_id on policy_versions are nullable — NULL
--     tenant_id means the version applies globally across all tenants; NULL
--     legal_entity_id means it applies to the whole tenant (or globally, if
--     tenant_id is also NULL).
--   - No RLS: policy_versions carries tenant_id but is filtered by
--     application-level scope match (policy_id, tenant_id, legal_entity_id),
--     not per-row RLS — reconsider if/when this service takes direct
--     multi-tenant traffic without a scope-aware caller.

-- ── policies ─────────────────────────────────────────────────────────────────

CREATE TABLE policies (
    policy_id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Stable, human-readable identifier — idempotent creation dedup key.
    -- DATA ONLY, never used as a code switch/case.
    policy_code             VARCHAR(128) NOT NULL,

    policy_name             TEXT        NOT NULL,

    -- VARCHAR — extensible via data migration only. E.g. APPROVAL_THRESHOLD,
    -- SPEND_CONTROL, SOD_RULE, SIGNATORY_MATRIX. Never a code switch/case for
    -- validation — only the evaluation handler switches on this, and only
    -- for the types it actually implements.
    policy_type             VARCHAR(64) NOT NULL,

    -- Audit
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT        NOT NULL
);

-- Idempotent creation key: same code = same policy.
CREATE UNIQUE INDEX idx_policies_code_unique ON policies (policy_code);

-- "Get applicable policy set" lookups filter by type.
CREATE INDEX idx_policies_type ON policies (policy_type);

-- ── policy_versions ──────────────────────────────────────────────────────────

CREATE TABLE policy_versions (
    policy_version_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id               UUID        NOT NULL REFERENCES policies(policy_id),

    -- NULL tenant_id = global (applies across all tenants).
    tenant_id               UUID,
    -- NULL legal_entity_id = applies to the whole tenant (or globally, if
    -- tenant_id is also NULL).
    legal_entity_id         UUID,

    -- Rule content. Shape depends on the owning policy's policy_type, e.g.
    -- {"threshold_amount": 5000} for APPROVAL_THRESHOLD.
    rule_payload            JSONB       NOT NULL DEFAULT '{}',

    -- Effective dating — point-in-time queries use:
    --   effective_from <= $at AND (effective_to IS NULL OR effective_to > $at)
    effective_from          TIMESTAMPTZ NOT NULL,
    effective_to            TIMESTAMPTZ,

    -- DRAFT | ACTIVE | SUPERSEDED | RETIRED — VARCHAR, not enum.
    version_status          VARCHAR(32) NOT NULL DEFAULT 'DRAFT',

    -- Audit
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT        NOT NULL
);

-- Idempotent creation key for a version within a policy+scope+effective_from.
-- COALESCE handles nullable tenant_id/legal_entity_id the same way
-- jurisdiction-rules-svc handles nullable parent_jurisdiction_id. This exact
-- expression list is targeted by name in pg_store.go's ON CONFLICT clause —
-- keep them in sync if either changes.
CREATE UNIQUE INDEX idx_policy_versions_dedup ON policy_versions (
    policy_id,
    COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
    COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
    effective_from
);

-- Primary lookup: applicable version(s) for a policy+scope.
CREATE INDEX idx_policy_versions_scope
    ON policy_versions (policy_id, tenant_id, legal_entity_id);

-- Enforce at most one ACTIVE version per (policy_id, tenant_id,
-- legal_entity_id) scope at any time. Activation must supersede the prior
-- ACTIVE version in the same transaction (superseding first) — see
-- PgStore.ActivateVersion — so this index is never violated mid-transaction.
CREATE UNIQUE INDEX idx_policy_versions_one_active_per_scope
    ON policy_versions (
        policy_id,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE version_status = 'ACTIVE';

-- History lookup, newest first.
CREATE INDEX idx_policy_versions_history
    ON policy_versions (policy_id, effective_from DESC);
