-- 000013_precedence_layers
--
-- INV-07: "Override precedence is schema-defined and deterministic". AA-001
-- names five layers — environment, service, tenant, organizational unit, user
-- preference — and storage held two (config_entries: environment and tenant).
-- The other three were declarable in allowed_scopes but refused at write time
-- because nothing could hold them.
--
-- They get their own append-only table rather than new columns on
-- config_entries, so the two existing layers — and every row, index and
-- policy they rely on — are untouched. Same history model: a change ends the
-- current row (effective_to) and inserts a new one; nothing is deleted.
--
-- Tenancy is fixed by layer: a SERVICE override is a per-service default
-- across tenants (tenant_id NULL, a platform-scope write); ORG_UNIT and
-- USER_PREFERENCE live inside one tenant.

CREATE TABLE config_layer_overrides (
    override_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    key                     VARCHAR(255) NOT NULL,
    environment             VARCHAR(64)  NOT NULL,
    tenant_id               UUID,
    layer                   VARCHAR(16)  NOT NULL
        CONSTRAINT chk_config_layer_overrides_layer CHECK (layer IN ('SERVICE','ORG_UNIT','USER_PREFERENCE')),
    scope_id                VARCHAR(255) NOT NULL
        CONSTRAINT chk_config_layer_overrides_scope CHECK (btrim(scope_id) <> ''),
    value                   JSONB        NOT NULL,
    effective_from          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    effective_to            TIMESTAMPTZ,
    created_by_principal_id TEXT         NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_config_layer_overrides_tenancy CHECK ((layer = 'SERVICE') = (tenant_id IS NULL))
);

-- One current value per (key, environment, tenant, layer, scope).
CREATE UNIQUE INDEX uq_config_layer_overrides_current
    ON config_layer_overrides (
        key, environment,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        layer, scope_id
    ) WHERE effective_to IS NULL;

ALTER TABLE config_layer_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_layer_overrides FORCE ROW LEVEL SECURITY;

-- config_entries' policy (000012), verbatim.
DROP POLICY IF EXISTS tenant_isolation_policy ON config_layer_overrides;
CREATE POLICY tenant_isolation_policy ON config_layer_overrides
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );

-- The snapshot manifest now imprints the three layers too. The environment,
-- tenant and flag branches are 000008's, unchanged.
CREATE OR REPLACE FUNCTION config_environment_manifest(p_environment TEXT)
RETURNS JSONB
LANGUAGE sql
STABLE
RETURN (
    SELECT COALESCE(jsonb_object_agg(e.entry_key, e.payload), '{}'::jsonb)
    FROM (
        SELECT
            c.key || '|' || COALESCE(c.tenant_id::text, '*') AS entry_key,
            jsonb_build_object(
                'key',                     c.key,
                'kind',                    'config',
                'value',                   c.value,
                'config_id',               c.config_id,
                'environment',             c.environment,
                'tenant_id',               c.tenant_id,
                'effective_from',          c.effective_from,
                'created_by_principal_id', c.created_by_principal_id
            ) AS payload
        FROM config_entries c
        WHERE c.environment = p_environment
          AND c.effective_to IS NULL

        UNION ALL

        -- INV-07: the SERVICE / ORG_UNIT / USER_PREFERENCE layers, keyed
        -- key|tenant|LAYER|scope so they never collide with the
        -- environment (key|*) and tenant (key|<tenant>) entries.
        SELECT
            o.key || '|' || COALESCE(o.tenant_id::text, '*') || '|' || o.layer || '|' || o.scope_id AS entry_key,
            jsonb_build_object(
                'key',                     o.key,
                'kind',                    'config',
                'value',                   o.value,
                'config_id',               o.override_id,
                'environment',             o.environment,
                'tenant_id',               o.tenant_id,
                'layer',                   o.layer,
                'scope_id',                o.scope_id,
                'effective_from',          o.effective_from,
                'created_by_principal_id', o.created_by_principal_id
            ) AS payload
        FROM config_layer_overrides o
        WHERE o.environment = p_environment
          AND o.effective_to IS NULL

        UNION ALL

        SELECT
            f.key || '|' || COALESCE(f.tenant_id::text, '*') AS entry_key,
            jsonb_build_object(
                'key',                     f.key,
                'kind',                    'flag',
                'flag_id',                 f.flag_id,
                'enabled',                 f.enabled,
                'rollout_percentage',      f.rollout_percentage,
                'environment',             f.environment,
                'tenant_id',               f.tenant_id,
                'effective_from',          f.effective_from,
                'created_by_principal_id', f.created_by_principal_id,
                'release_plan',            rp.plan,
                'kill_switch',             ks.switch
            ) AS payload
        FROM feature_flags f
        LEFT JOIN LATERAL (
            SELECT jsonb_build_object(
                'release_plan_id', p.release_plan_id,
                'version',         p.version,
                'strategy',        p.strategy,
                'salt',            p.salt,
                'bucket_count',    p.bucket_count,
                'targeting_hash',  p.targeting_hash,
                'targeting_rules', p.targeting_rules
            ) AS plan
            FROM release_plans p
            WHERE p.flag_key = f.key
              AND p.environment = f.environment
              AND p.tenant_id IS NOT DISTINCT FROM f.tenant_id
            ORDER BY p.version DESC
            LIMIT 1
        ) rp ON true
        LEFT JOIN LATERAL (
            SELECT jsonb_build_object(
                'kill_switch_id', k.kill_switch_id,
                'safe_behavior',  k.safe_behavior,
                'reason',         k.reason,
                'incident_id',    k.incident_id,
                'expires_at',     k.expires_at
            ) AS switch
            FROM kill_switches k
            WHERE k.flag_key = f.key
              AND k.environment = f.environment
              AND k.expired_at IS NULL
              AND k.expires_at > NOW()
              AND (k.tenant_id IS NOT DISTINCT FROM f.tenant_id OR k.tenant_id IS NULL)
            ORDER BY (k.tenant_id IS NULL) ASC, k.created_at DESC
            LIMIT 1
        ) ks ON true
        WHERE f.environment = p_environment
          AND f.effective_to IS NULL
    ) e(entry_key, payload)
);

