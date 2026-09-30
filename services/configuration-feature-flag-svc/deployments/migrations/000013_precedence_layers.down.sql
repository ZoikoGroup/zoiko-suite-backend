-- Restores 000008's manifest function, then drops the layer table.
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

DROP TABLE IF EXISTS config_layer_overrides;
