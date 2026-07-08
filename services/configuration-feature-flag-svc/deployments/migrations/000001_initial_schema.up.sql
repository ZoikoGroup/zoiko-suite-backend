-- 000001_initial_schema.up.sql
-- Configuration & Feature Flag Service — initial schema
--
-- Owns:
--   config_entries  — versioned, effective-dated runtime config values
--   feature_flags   — versioned, effective-dated feature flags
--
-- Design (docs/architecture/03-microservices.md §9.6 has no schema at all
-- — this is entirely the approved build task's design, see context.md §7):
--   - No soft-delete, no UPDATE/DELETE on either table. A "change" is
--     always: end-date the row currently effective for a given
--     (key, environment, tenant_id) scope, and insert a new row, in the
--     same transaction.
--   - tenant_id is nullable — NULL means the global default for that
--     environment.
--   - A partial unique index enforces at most one currently-effective row
--     (effective_to IS NULL) per (key, environment, tenant_id) scope —
--     this is the concurrency backstop the upsert transaction logic
--     relies on, not just an optimization.

-- ── config_entries ───────────────────────────────────────────────────────────

CREATE TABLE config_entries (
    config_id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    key                     VARCHAR(255) NOT NULL,

    -- Structured config value. JSONB chosen over TEXT so structured
    -- values (objects/arrays/numbers/booleans) don't need caller-side
    -- encoding tricks — mirrors rule_payload's precedent in policy-svc.
    value                   JSONB       NOT NULL,

    environment             VARCHAR(64) NOT NULL,

    -- NULL tenant_id = global default for this environment.
    tenant_id               UUID,

    -- Effective dating — the row with effective_to IS NULL for a given
    -- (key, environment, tenant_id) scope is "currently effective".
    effective_from          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to            TIMESTAMPTZ,

    -- Audit
    created_by_principal_id TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one currently-effective row per (key, environment, tenant_id)
-- scope. COALESCE handles nullable tenant_id the same way policy-svc
-- handles nullable tenant_id/legal_entity_id in its own partial index.
CREATE UNIQUE INDEX idx_config_entries_one_effective_per_scope
    ON config_entries (
        key,
        environment,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE effective_to IS NULL;

-- Primary lookup: currently-effective entry for a given scope, and the
-- list-by-environment/tenant query.
CREATE INDEX idx_config_entries_scope
    ON config_entries (key, environment, tenant_id);

-- ── feature_flags ────────────────────────────────────────────────────────────

CREATE TABLE feature_flags (
    flag_id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    key                     VARCHAR(255) NOT NULL,

    enabled                 BOOLEAN     NOT NULL,

    environment             VARCHAR(64) NOT NULL,

    -- NULL tenant_id = global default for this environment.
    tenant_id               UUID,

    rollout_percentage      INTEGER     NOT NULL DEFAULT 100
        CONSTRAINT chk_feature_flags_rollout_percentage_range
            CHECK (rollout_percentage BETWEEN 0 AND 100),

    effective_from          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to            TIMESTAMPTZ,

    created_by_principal_id TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_feature_flags_one_effective_per_scope
    ON feature_flags (
        key,
        environment,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE effective_to IS NULL;

CREATE INDEX idx_feature_flags_scope
    ON feature_flags (key, environment, tenant_id);
