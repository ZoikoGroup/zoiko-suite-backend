-- 20260818000900_configuration_feature_flag_svc.sql
-- configuration-feature-flag-svc → schema `configuration_feature_flag`
--
-- End state of 000001_initial_schema (the service's only migration).
-- Two tables: config_entries, feature_flags.
--
-- ── Design carried over ──────────────────────────────────────────────────────
--   - No soft-delete and no UPDATE/DELETE on either table. A "change" is always
--     end-date the currently-effective row for a (key, environment, tenant_id)
--     scope and insert a new one, in the same transaction.
--   - tenant_id is NULLABLE: NULL means the global default for that environment.
--   - The partial unique index is a CONCURRENCY BACKSTOP the upsert transaction
--     relies on, not an optimisation.
--
-- ── The nullable tenant_id needs a policy shaped differently ─────────────────
-- Every other service in this set has NOT NULL tenant_id and a plain equality
-- policy. Here a NULL means "global", so reads must return global rows as well
-- as the caller's own — and that makes the WRITE side the interesting half.
--
-- If the write policy simply mirrored the read policy, ANY tenant could insert
-- a row with tenant_id NULL and change a global default for every other tenant.
-- That is a privilege escalation dressed as a config write. So the two clauses
-- are deliberately asymmetric:
--
--   USING      — global rows OR my own tenant's rows.
--   WITH CHECK — my own tenant's rows; a GLOBAL row may be written only by a
--                connection that has installed NO tenant at all.
--
-- The second half of that WITH CHECK is a narrow, deliberate carve-out to the
-- fail-closed rule the rest of this migration set follows. It is safe because
-- it grants strictly less than the alternative: a tenant connection can never
-- write a global, and a no-tenant connection can write ONLY globals (its own
-- tenant clause is NULL, which is not true). Platform-level config therefore
-- requires an explicitly platform-level connection.

CREATE SCHEMA IF NOT EXISTS configuration_feature_flag;

COMMENT ON SCHEMA configuration_feature_flag IS
    'configuration-feature-flag-svc. Versioned, effective-dated runtime config and feature flags. NULL tenant_id = global default for the environment.';

GRANT USAGE ON SCHEMA configuration_feature_flag TO zoiko_backend, authenticated;

-- ── config_entries ───────────────────────────────────────────────────────────

CREATE TABLE configuration_feature_flag.config_entries (
    config_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    key                     VARCHAR(255) NOT NULL,

    -- JSONB over TEXT so structured values (objects, arrays, numbers, booleans)
    -- need no caller-side encoding tricks.
    value                   JSONB        NOT NULL,

    environment             VARCHAR(64)  NOT NULL,

    -- NULL = global default for this environment.
    tenant_id               UUID,

    -- The row with effective_to IS NULL for a given scope is the currently
    -- effective one.
    effective_from          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    effective_to            TIMESTAMPTZ,

    created_by_principal_id TEXT         NOT NULL DEFAULT app.current_principal_id(),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT config_entries_period_ordered
        CHECK (effective_to IS NULL OR effective_to > effective_from)
);

-- At most one currently-effective row per (key, environment, tenant_id) scope.
-- COALESCE so a NULL tenant participates rather than making every global row
-- distinct from every other — without it the "one effective global" rule would
-- not hold at all.
CREATE UNIQUE INDEX idx_config_entries_one_effective_per_scope
    ON configuration_feature_flag.config_entries (
        key,
        environment,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE effective_to IS NULL;

CREATE INDEX idx_config_entries_scope
    ON configuration_feature_flag.config_entries (key, environment, tenant_id);

-- ── feature_flags ────────────────────────────────────────────────────────────

CREATE TABLE configuration_feature_flag.feature_flags (
    flag_id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    key                     VARCHAR(255) NOT NULL,
    enabled                 BOOLEAN      NOT NULL,
    environment             VARCHAR(64)  NOT NULL,

    -- NULL = global default for this environment.
    tenant_id               UUID,

    rollout_percentage      INTEGER      NOT NULL DEFAULT 100
        CONSTRAINT chk_feature_flags_rollout_percentage_range
            CHECK (rollout_percentage BETWEEN 0 AND 100),

    effective_from          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    effective_to            TIMESTAMPTZ,

    created_by_principal_id TEXT         NOT NULL DEFAULT app.current_principal_id(),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT feature_flags_period_ordered
        CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE UNIQUE INDEX idx_feature_flags_one_effective_per_scope
    ON configuration_feature_flag.feature_flags (
        key,
        environment,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE effective_to IS NULL;

CREATE INDEX idx_feature_flags_scope
    ON configuration_feature_flag.feature_flags (key, environment, tenant_id);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE configuration_feature_flag.config_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE configuration_feature_flag.config_entries FORCE  ROW LEVEL SECURITY;
ALTER TABLE configuration_feature_flag.feature_flags  ENABLE ROW LEVEL SECURITY;
ALTER TABLE configuration_feature_flag.feature_flags  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON configuration_feature_flag.config_entries
    FOR ALL
    TO zoiko_backend
    USING (
        tenant_id IS NULL
        OR tenant_id::text = app.current_tenant_id()
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    );

CREATE POLICY tenant_read ON configuration_feature_flag.config_entries
    FOR SELECT
    TO authenticated
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_isolation ON configuration_feature_flag.feature_flags
    FOR ALL
    TO zoiko_backend
    USING (
        tenant_id IS NULL
        OR tenant_id::text = app.current_tenant_id()
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    );

CREATE POLICY tenant_read ON configuration_feature_flag.feature_flags
    FOR SELECT
    TO authenticated
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON configuration_feature_flag.config_entries TO authenticated;
GRANT SELECT ON configuration_feature_flag.feature_flags  TO authenticated;

-- UPDATE is granted because a change end-dates the currently-effective row
-- (setting effective_to) and inserts its replacement in the same transaction.
-- DELETE is granted to nobody: there is no soft-delete and no hard delete.
GRANT SELECT, INSERT, UPDATE ON configuration_feature_flag.config_entries TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON configuration_feature_flag.feature_flags  TO zoiko_backend;
