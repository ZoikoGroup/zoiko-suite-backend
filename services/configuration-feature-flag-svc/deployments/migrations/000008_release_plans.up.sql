-- Migration: 000008_release_plans.up.sql
--
-- Release plans, flag retirement, and the outbox CHECK widened to admit the
-- eight AA-001 events.
--
-- WHY (ZS-SVC-AA-001, §10.1 POST /flags/{id}/release-plans, §6.2/6.3,
-- Table 16/17, INV-07/18/19/20/21, NP-13/14/15/19/20):
--
--   release_plans  — rollout/targeting plans stored as immutable versions.
--                    Eligibility filters are evaluated BEFORE any percentage
--                    (§6.2 — "10% of all users" is invalid where tenant,
--                    region, plan or policy restrictions apply); bucketing is
--                    deterministic for a stable subject key + salt version so
--                    the same subject does not oscillate variants (INV-07);
--                    targeting rules are immutable per published plan version
--                    (INV-04); temporary flags must carry owner + created date
--                    + retirement deadline (INV-20, NP-19).
--   flag_retirements — Table 17 retirement/removal: a retired flag's key is
--                    tombstoned so it cannot be immediately reused for
--                    different semantics (INV-21, NP-20). `reusable` stays
--                    false until the verified consumer scan clears it (TC-16).
--   outbox CHECK    — event_outbox's CHECK currently admits only
--                    config.updated and feature_flag.updated. 000003's own
--                    comments say "keep in step with internal/events and
--                    asyncapi.yaml"; 000008 adds the eight spec events, so
--                    the constraint must admit all ten or the enqueue (and
--                    therefore the write) would fail the moment an event
--                    builder lands.
--
-- RLS: release plans are read by flag evaluation across tenants (the plan's
-- targeting rules must be visible to the resolver), so USING (true) with the
-- same tenant-scoped write check as 000006's ops tables; retirements are
-- global metadata read by the write gate (NP-20) and written by the
-- retirement operation behind app.ops_sweep.

-- ── release_plans ─────────────────────────────────────────────────────────────

CREATE TABLE release_plans (
    release_plan_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The flag this plan governs, identified by KEY, not by a feature_flags
    -- row id: a flag value write end-dates the old row and inserts a new one
    -- with a NEW flag_id (000001's append-only doctrine), so a plan bound to
    -- a row id would be orphaned by the very first value update. The key is
    -- the stable identity across every effective row and every tenant scope.
    flag_key             VARCHAR(255) NOT NULL,

    environment          VARCHAR(64) NOT NULL,
    tenant_id            UUID,

    -- PERCENTAGE / PROGRESSIVE_SCHEDULE / ALL_OR_NOTHING (INV-19: no
    -- randomized experimentation for regulated/rights-affecting outcomes —
    -- those use ALL_OR_NOTHING).
    strategy             VARCHAR(32) NOT NULL
        CONSTRAINT chk_release_plans_strategy CHECK (
            strategy IN ('PERCENTAGE','PROGRESSIVE_SCHEDULE','ALL_OR_NOTHING')
        ),

    -- Deterministic bucketing salt; changing it deliberately reshuffles
    -- cohorts while keeping stability across evaluations for a given salt.
    salt                 VARCHAR(255) NOT NULL,
    bucket_count         INTEGER     NOT NULL DEFAULT 1000
        CONSTRAINT chk_release_plans_bucket_count CHECK (bucket_count BETWEEN 1 AND 1000000),

    -- Hash of the published targeting rules — pinned in evaluation evidence
    -- (TC-05) so a past evaluation is reproducible to the exact ruleset.
    targeting_hash       VARCHAR(64) NOT NULL,

    -- The immutable rules: eligibility filters evaluated first, then rollout
    -- percentage, variants and schedule steps. JSONB on purpose — the exact
    -- shape is the plan's own contract (Table 24: "Eligibility first;
    -- deterministic buckets; experiment restrictions").
    targeting_rules      JSONB       NOT NULL,

    -- Immutable once published (INV-04); edits are a new version.
    version              INTEGER     NOT NULL DEFAULT 1,
    published_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_by_principal_id TEXT   NOT NULL,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_release_plans_one_version_per_scope UNIQUE (
        flag_key,
        environment,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        version
    )
);

CREATE INDEX idx_release_plans_lookup
    ON release_plans (environment, flag_key, version DESC);

-- ── flag_retirements ──────────────────────────────────────────────────────────

CREATE TABLE flag_retirements (
    retirement_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    key                  VARCHAR(255) NOT NULL,
    environment          VARCHAR(64) NOT NULL,
    tenant_id            UUID,

    -- Final intended value the flag is forced to at retirement (Table 17:
    -- "Flag forced to final intended value and targeting disabled").
    final_enabled        BOOLEAN     NOT NULL,
    final_rollout        INTEGER     NOT NULL DEFAULT 100
        CONSTRAINT chk_flag_retirements_final_rollout CHECK (final_rollout BETWEEN 0 AND 100),

    -- Consumer/dependency verification (INV-21, NP-20): REMOVED is not the
    -- same as RETIRED. The key cannot be reused for a different semantic
    -- meaning until `reusable` is set after the consumer scan clears.
    reusable             BOOLEAN     NOT NULL DEFAULT false,
    consumer_scan_evidence JSONB,

    retired_by_principal_id TEXT        NOT NULL,
    retired_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at              TIMESTAMPTZ,

    CONSTRAINT uq_flag_retirements_key UNIQUE (key, environment)
);

-- ── Widen the outbox event CHECK to all ten event types ──────────────────────
-- DROP + re-CREATE, because Postgres has no ALTER CONSTRAINT. Matches the
-- ten builders 000008's companion event file adds to internal/events and the
-- asyncapi.yaml channels.

ALTER TABLE event_outbox DROP CONSTRAINT event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'config.updated',
        'feature_flag.updated',
        'config.snapshot.published',
        'config.version.published',
        'config.override.activated',
        'flag.release.activated',
        'flag.kill_switch.activated',
        'config.change.verified',
        'config.drift.detected',
        'config.emergency.expired'
    ));

-- ── RLS ───────────────────────────────────────────────────────────────────────

ALTER TABLE release_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE release_plans FORCE ROW LEVEL SECURITY;
ALTER TABLE flag_retirements ENABLE ROW LEVEL SECURITY;
ALTER TABLE flag_retirements FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS release_plans_read_all_write_scoped ON release_plans;
CREATE POLICY release_plans_read_all_write_scoped ON release_plans FOR ALL
    USING (true)
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS flag_retirements_read_all_write_admin ON flag_retirements;
CREATE POLICY flag_retirements_read_all_write_admin ON flag_retirements FOR ALL
    USING (true)
    WITH CHECK (
        COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.definition_admin', true), ''), 'false') = 'true'
    );

-- ── config_environment_manifest() ─────────────────────────────────────────────
-- The ONE manifest builder, created here — and only here — so it has a single
-- definition with its final shape, reading every table the snapshot must
-- cover (config_entries, feature_flags, and their active kill switches and
-- latest release plans). 000004 deferred it here deliberately: between 000004
-- and 000008 no code runs and no table it reads exists.
--
-- Manifest shape: a JSONB object keyed `key|tenant_id` (tenant_id '*'
-- standing for the global default for the environment), each value an object
-- discriminated by `kind`:
--   config — value + the evidence to explain it (effective_from, actor);
--   flag   — enabled/rollout plus the CURRENT release plan and active kill
--            switch, embedded so evaluation is fully pinned to this snapshot
--            (INV-12: evaluation evidence must be reproducible from the
--            imprint alone — a later plan edit or a switch flip can never
--            rewrite what an already-issued snapshot said).
-- The mint calls it over live rows under app.snapshot_mint; every read path
-- takes the stored content instead of re-deriving from live rows.
--
-- STABLE, not IMMUTABLE — it reads tables. SECURITY INVOKER by default, so
-- it sees exactly what its caller's session is entitled to see; the mint
-- session is admitted by the app.snapshot_mint escape that 000004 patched
-- into the config_entries / feature_flags policies. The kill-switch lookup
-- prefers the tenant-specific switch over the environment-wide one; the
-- release-plan lookup takes the latest version for the flag's exact scope.

CREATE FUNCTION config_environment_manifest(p_environment TEXT)
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

-- ── Snapshot backfill for every environment ───────────────────────────────────
-- First imprint for each environment that already holds effective
-- configuration (or flags), epoch 1. Everything below is created by this
-- batch in one deployment, so this is the only backfill the snapshot tables
-- ever need. Runs with app.snapshot_mint at session level, for the same
-- reason 000004 documented: the manifest reads config_entries / feature_flags
-- under their patched policies, and the INSERT below must pass the WITH CHECK.

DO $backfill$
DECLARE
    r        RECORD;
    manifest JSONB;
BEGIN
    PERFORM set_config('app.snapshot_mint', 'true', false);

    FOR r IN
        SELECT environment AS env FROM config_entries WHERE effective_to IS NULL
        UNION
        SELECT environment AS env FROM feature_flags WHERE effective_to IS NULL
    LOOP
        INSERT INTO config_snapshot_epochs (environment, current_epoch)
        VALUES (r.env, 1)
        ON CONFLICT (environment) DO NOTHING;

        -- Computed once so that the stored digest is, by construction,
        -- md5() of the stored content — a reader re-hashing the content it
        -- fetched can never disagree with this row.
        manifest := config_environment_manifest(r.env);

        INSERT INTO config_snapshots (environment, epoch, digest, content, created_by_principal_id)
        VALUES (r.env, 1, md5(manifest::text), manifest, 'system:migration-000008')
        ON CONFLICT (environment, epoch) DO NOTHING;
    END LOOP;
END
$backfill$;