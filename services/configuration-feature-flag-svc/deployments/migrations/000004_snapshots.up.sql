-- Migration: 000004_snapshots.up.sql
--
-- Immutable, epoch-dated snapshots of effective configuration, plus the
-- canonical manifest function that builds them.
--
-- WHY (ZS-SVC-AA-001, INV-12, INV-22, INV-29, Table 12, §5.1):
--
-- The service's only read path — `GET /v1/config/{key}` — resolves a value by
-- selecting the row that is *currently* effective from a table that admin
-- writes mutate on every change. INV-12 forbids exactly that: "Runtime
-- evaluation pins an exact snapshot/version; mutable admin records are never
-- read as execution truth." A value read at 10:00:01 and a value read at
-- 10:00:02 can answer "what sent the payroll batch out?" differently, and
-- nothing later can prove which one was in force when it happened.
--
-- The spec's apparatus for this is the immutable snapshot manifest (Table 6
-- "ResolvedConfigSnapshot", §9.1): a point-in-time imprint of the effective
-- configuration for an environment, carrying an epoch, a digest and a
-- freshness deadline. Evaluation reads the imprint; admin rows stay admin
-- rows. This migration provides:
--
--   config_snapshot_epochs  — one row per environment, a monotonic epoch
--                             counter. The mint bumps it via
--                             INSERT ... ON CONFLICT DO UPDATE, whose row
--                             lock serializes concurrent mints *per
--                             environment*, so two racing mints can never
--                             both claim the same epoch (INV-22 — monotonic,
--                             no silent overwrite of newer by older).
--   config_snapshots        — the imprints themselves: the manifest JSONB,
--                             an md5 digest over its canonical serialization,
--                             issued_at and a freshness_deadline.
--                             UNIQUE (environment, epoch) is the database
--                             backstop behind the counter.
--   config_environment_manifest() — the ONE SQL function shared by mint and
--                             reconstruction. It is created in 000008, after
--                             every table it reads (config_entries,
--                             feature_flags, kill_switches, release_plans)
--                             exists, so it is defined exactly once with its
--                             final shape; the epoch/snapshot backfill runs
--                             there too, against that one definition. Nothing
--                             between 000004 and 000008 needs it — every
--                             migration in this batch is applied before any
--                             request handler starts, so a read path never
--                             observes a schema without the function.
--
-- RLS:
--
-- config_snapshots and config_snapshot_epochs are FORCE RLS, but they are
-- read by every evaluation request (no tenant owns an environment-wide
-- imprint), so the read side admits all — USING (true). Writes only ever
-- come from the mint, which is not a request but a transaction-local helper
-- in the store that names itself through the app.snapshot_mint GUC in the
-- WITH CHECK. This mirrors event_outbox's relay escape in 000003 exactly:
-- an explicit, auditable disjunct instead of "a connection that forgot its
-- guard can't write" (which under FORCE would present as a snapshot that
-- never gets created while reporting no error).
--
-- The mint must also be able to READ every tenant's rows — a global default
-- and its tenant overrides are all part of the same environment imprint —
-- so the existing tenant_isolation_policy on config_entries and feature_flags
-- gains a third USING/WITH CHECK disjunct for app.snapshot_mint. The NULLIF
-- branches that admission is built on are preserved verbatim. This is the
-- only cross-tenant reader this service has, and it is a system path, not a
-- request path; no request handler ever sets app.snapshot_mint.

-- ── config_snapshot_epochs ────────────────────────────────────────────────────
-- The per-environment epoch counter. The row for an environment is created on
-- the environment's first mint and locked by later mints through the
-- DO UPDATE on conflict, which is what serializes them.

CREATE TABLE config_snapshot_epochs (
    environment     VARCHAR(64) PRIMARY KEY,
    current_epoch   BIGINT      NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── config_snapshots ──────────────────────────────────────────────────────────
-- The immutable imprints. content is the output of the manifest function;
-- digest is md5(content::text) — md5 is built in, deterministic across
-- JSONB-by-value equality (a rebuild of the same effective rows serializes
-- byte-identically), and is the same hashing strategy the outbox relay's
-- deduplication relies on elsewhere in this repo. No pgcrypto extension is
-- warranted for a defensive integrity check whose real signing story is OD-03.

CREATE TABLE config_snapshots (
    snapshot_id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- NULL tenant_id BY DESIGN. An imprint covers an environment in full —
    -- its globals and every tenant's overrides — and carries the tenant
    -- dimension *inside* content, keyed `key|tenant_id`. A snapshot is never
    -- scoped to one tenant the way a config_entries row is.
    environment                 VARCHAR(64) NOT NULL,
    epoch                       BIGINT      NOT NULL,

    -- md5 hex, 32 chars, of content::text. 64 wide to leave headroom for a
    -- future sha256 without a migration.
    digest                      VARCHAR(64) NOT NULL,

    content                     JSONB       NOT NULL,

    issued_at                   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Per-snapshot expiry. NOW() is the start-of-transaction time, so a
    -- freshly inserted row holds issued_at and freshness_deadline that are
    -- exactly 24h apart. 24h is a placeholder standing in for OD-04's
    -- quantitative freshness budgets by safety class; the constant lives here
    -- so it is visible in one place when OD-04 closes.
    freshness_deadline          TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '24 hours',

    -- Who caused this mint. The store passes the triggering writer's
    -- principal; every snapshot is thereby attributable (INV-14).
    created_by_principal_id     TEXT        NOT NULL,

    CONSTRAINT uq_config_snapshots_environment_epoch UNIQUE (environment, epoch)
);

ALTER TABLE config_snapshot_epochs ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_snapshot_epochs FORCE ROW LEVEL SECURITY;
ALTER TABLE config_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_snapshots FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS snapshot_epochs_mint_only ON config_snapshot_epochs;
CREATE POLICY snapshot_epochs_mint_only ON config_snapshot_epochs FOR ALL
    USING (true)
    WITH CHECK (
        COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS snapshots_mint_only ON config_snapshots;
CREATE POLICY snapshots_mint_only ON config_snapshots FOR ALL
    USING (true)
    WITH CHECK (
        COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
    );

-- ── Patch tenant_isolation_policy on config_entries / feature_flags ──────────
-- Add the mint's read escape. Dropped first so the whole migration is
-- re-runnable, matching the established pattern. The two original NULLIF
-- branches are reproduced exactly — a global row stays readable by every
-- tenant and a tenant row stays confined to its tenant — so the migration
-- only ever adds, never changes, the request-path isolation 000002 created.

DROP POLICY IF EXISTS tenant_isolation_policy ON config_entries;
CREATE POLICY tenant_isolation_policy ON config_entries
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON feature_flags;
CREATE POLICY tenant_isolation_policy ON feature_flags
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
    );