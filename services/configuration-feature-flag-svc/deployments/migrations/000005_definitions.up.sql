-- Migration: 000005_definitions.up.sql
--
-- The key registry: config_definitions (the working declaration, one row per
-- config key) and config_definition_versions (immutable published snapshots
-- of a definition at every publish).
--
-- WHY (ZS-SVC-AA-001, INV-05/06/08, NP-01/02/03/05/07, Table 11, Table 10):
--
-- Today a key is whatever string a caller's POST body happens to contain —
-- free-form strings with no type, no owner, no allowed scopes, no fallback
-- policy and no safety class. That one missing entity is the root of INV-05
-- ("unknown keys are accepted"), INV-06 (no declaration surface), INV-08 (no
-- scope allowlist) and NP-01/02/03/05/07, all of which the audit traces back
-- to "no key declaration". Table 10 makes the intent explicit: "A runtime
-- must never infer configuration semantics from an ad-hoc JSON blob". Table 11
-- then fixes the fields a ConfigDefinition must carry.
--
-- Two tables instead of one because INV-04's immutability ("Published
-- ConfigVersion objects are immutable") does not fit a single editable row.
-- config_definitions is the *working* declaration: it can be edited and
-- transition DRAFT -> REVIEW while unpublished. The instant it reaches
-- PUBLISHED the store copies the full declaration into an immutable
-- config_definition_version row with a digest; from then on, resolve, the
-- write gate, and the snapshots they feed all read the versions, never the
-- working row. Deprecation and retirement are themselves publications (a new
-- version carrying the new lifecycle), not edits to history.
--
-- Resolution depth: config_values live at (key, environment, tenant) but a
-- definition is scoped by the SCOPES it permits, not by any tenant — it is
-- shared, platform metadata that every request needs to resolve values. So
-- both tables are FORCE RLS with USING (true) — read by everyone — and the
-- write side is confined to the store's definition operations, which
-- transaction-locally set the app.definition_admin GUC, admitted by the
-- WITH CHECK. Same explicit-escape doctrine as 000004's app.snapshot_mint
-- and 000003's app.outbox_relay: never a policy with no named writer.

-- ── config_definitions ────────────────────────────────────────────────────────
-- The working declaration. One row per key; unique key is the registry's
-- spine (a key name is globally stable for one semantic meaning, Table 11).

CREATE TABLE config_definitions (
    definition_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Globally stable within this namespace; lowercase dotted form.
    key                  VARCHAR(255) NOT NULL,

    -- Service/team plus accountable business/security owner.
    owner                TEXT        NOT NULL,

    value_type           VARCHAR(32) NOT NULL
        CONSTRAINT chk_config_definitions_value_type CHECK (
            value_type IN ('BOOLEAN','INTEGER','DECIMAL','STRING','ENUM',
                           'DURATION','URI','CIDR_SET','STRING_SET','STRUCTURED')
        ),

    -- S0 cosmetic; S1 operational low risk; S2 material tenant behavior;
    -- S3 security/privacy/financial/regulated adjacent.
    safety_class         VARCHAR(4)  NOT NULL
        CONSTRAINT chk_config_definitions_safety_class CHECK (
            safety_class IN ('S0','S1','S2','S3')
        ),

    -- Explicit ordered set of scopes allowed to override this key (INV-08).
    -- Array of ENVIRONMENT / TENANT / etc., kept as JSONB to preserve order.
    allowed_scopes       JSONB       NOT NULL,

    -- Typed safe default, or JSONB null meaning "REQUIRED, no default".
    default_value        JSONB,

    -- Per-key failure semantics (INV-28): what a consumer may do when the
    -- control plane is unreachable or the value is stale.
    fallback_policy      VARCHAR(32) NOT NULL
        CONSTRAINT chk_config_definitions_fallback_policy CHECK (
            fallback_policy IN ('USE_CACHED_WITH_MAX_AGE','SAFE_DEFAULT','BLOCK','DEGRADE')
        ),

    -- PUBLIC_CONFIG / INTERNAL / RESTRICTED_METADATA / SECRET_REFERENCE_ONLY.
    -- SECRET_REFERENCE_ONLY is the guardrail for INV-09: the value of such a
    -- key is a secret *reference* ("secret://provider/path"), never material.
    sensitivity          VARCHAR(32) NOT NULL
        CONSTRAINT chk_config_definitions_sensitivity CHECK (
            sensitivity IN ('PUBLIC_CONFIG','INTERNAL','RESTRICTED_METADATA','SECRET_REFERENCE_ONLY')
        ),

    -- Bounds/regex/enum/dependency constraints, as a schema the store and
    -- consumers can read (NP-03: a boolean key may not accept a string).
    validation           JSONB,

    -- IMMEDIATE / SCHEDULED / PERIOD (effective-dated); history always kept.
    effective_model      VARCHAR(16) NOT NULL DEFAULT 'IMMEDIATE'
        CONSTRAINT chk_config_definitions_effective_model CHECK (
            effective_model IN ('IMMEDIATE','SCHEDULED','PERIOD')
        ),

    -- DRAFT -> REVIEW -> PUBLISHED -> DEPRECATED -> RETIRED (Table 7).
    lifecycle            VARCHAR(16) NOT NULL DEFAULT 'DRAFT'
        CONSTRAINT chk_config_definitions_lifecycle CHECK (
            lifecycle IN ('DRAFT','REVIEW','PUBLISHED','DEPRECATED','RETIRED')
        ),

    -- Replacement key, migration plan, consumer inventory, removal conditions.
    deprecation          JSONB,

    -- Flag-specific definition fields (Table 6 FlagDefinition, Table 16
    -- classes, INV-20/NP-19). NULL for config keys and for flag keys whose
    -- class predates this registry (the backfill must not invent a class it
    -- cannot know). When a flag class IS declared and it is temporary —
    -- RELEASE / MIGRATION / EXPERIMENT / COMPATIBILITY / OPS_KILL_SWITCH —
    -- the write gate requires retirement_deadline to be set; a temporary
    -- flag without owner, created date or retirement deadline is rejected
    -- exactly as NP-19 demands. PERMANENT is the explicit "no deadline"
    -- class, never an implicit absence.
    flag_class           VARCHAR(32)
        CONSTRAINT chk_config_definitions_flag_class CHECK (
            flag_class IS NULL OR flag_class IN
                ('RELEASE','OPS_KILL_SWITCH','MIGRATION','EXPERIMENT','COMPATIBILITY','PERMANENT')
        ),
    retirement_deadline  TIMESTAMPTZ,

    created_by_principal_id TEXT        NOT NULL,
    updated_by_principal_id TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_config_definitions_key UNIQUE (key)
);

-- ── config_definition_versions ────────────────────────────────────────────────
-- Immutable published artifacts. A publish copies the full declaration into
-- `definition`, digests it, and pins the lifecycle the publish produced. No
-- UPDATE/DELETE ever touches this table — INV-04 (NP-04) is enforced by
-- absence of write intent, same doctrine as the append-only value tables.
-- Versions are per-definition sequential, so reconstructing "what was key K
-- at version N" needs no guesswork.

CREATE TABLE config_definition_versions (
    version_id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    definition_id         UUID        NOT NULL REFERENCES config_definitions(definition_id),

    -- Per-definition sequential version number, checked by the store against
    -- the working row's current state before insert.
    version               INTEGER     NOT NULL,

    -- md5 of definition::text, same rationale as config_snapshots.digest.
    digest                VARCHAR(64) NOT NULL,

    -- The immutable declaration, complete and self-describing.
    definition            JSONB       NOT NULL,

    -- The lifecycle this publication produced. Only PUBLISHED / DEPRECATED /
    -- RETIRED are publishable states; DRAFT and REVIEW are pre-publication.
    lifecycle             VARCHAR(16) NOT NULL
        CONSTRAINT chk_config_definition_versions_lifecycle CHECK (
            lifecycle IN ('PUBLISHED','DEPRECATED','RETIRED')
        ),

    published_by_principal_id TEXT        NOT NULL,
    published_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_config_definition_versions_definition_version
        UNIQUE (definition_id, version)
);

CREATE INDEX idx_config_definition_versions_lookup
    ON config_definition_versions (definition_id, version DESC);

ALTER TABLE config_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_definitions FORCE ROW LEVEL SECURITY;
ALTER TABLE config_definition_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_definition_versions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS definitions_read_all_write_admin ON config_definitions;
CREATE POLICY definitions_read_all_write_admin ON config_definitions FOR ALL
    USING (true)
    WITH CHECK (
        COALESCE(NULLIF(current_setting('app.definition_admin', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS definition_versions_read_all_write_admin ON config_definition_versions;
CREATE POLICY definition_versions_read_all_write_admin ON config_definition_versions FOR ALL
    USING (true)
    WITH CHECK (
        COALESCE(NULLIF(current_setting('app.definition_admin', true), ''), 'false') = 'true'
    );

-- ── Backfill ──────────────────────────────────────────────────────────────────
-- Every key already in use becomes a PUBLISHED definition (working row) plus
-- its immutable version 1. This is what keeps INV-05 from breaking the store
-- the moment the write gate lands: keys that predate the registry are not
-- "unknown" — they are provisioned by this migration out of the values that
-- already exist. New undeclared keys are then refused exactly as INV-05 wants.
--
-- Types are inferred from the shape actually stored (jsonb_typeof), because
-- a backfill must not pretend to know more than its data does:
--   boolean -> BOOLEAN          string -> STRING
--   number  -> DECIMAL          object/array/null -> STRUCTURED
-- A pre-existing defining safety class or scope allowlist does not exist, so
-- S1 is the honest default (operational, low risk), ENVIRONMENT+TENANT the
-- actual scopes the value tables can hold, fallback BLOCK (never silently
-- answer with a stale/invented value), sensitivity INTERNAL (config values we
-- cannot swear are public), effective model IMMEDIATE.

CREATE OR REPLACE FUNCTION _cff000005_infer_value_type(v JSONB)
RETURNS VARCHAR(32)
LANGUAGE sql
IMMUTABLE
RETURN (
    CASE jsonb_typeof(v)
        WHEN 'boolean' THEN 'BOOLEAN'
        WHEN 'string'  THEN 'STRING'
        WHEN 'number'  THEN 'DECIMAL'
        ELSE 'STRUCTURED'
    END
);

DO $backfill$
DECLARE
    r          RECORD;
    def_id     UUID;
    inf_type   VARCHAR(32);
    def_json   JSONB;
    v_digest   VARCHAR(64);
BEGIN
    PERFORM set_config('app.definition_admin', 'true', false);

    FOR r IN
        SELECT
            source.key,
            source.value,
            ROW_NUMBER() OVER (PARTITION BY source.key ORDER BY source.updated_at DESC) AS rn
        FROM (
            SELECT key, value, created_at AS updated_at
            FROM config_entries WHERE effective_to IS NULL
            UNION ALL
            SELECT key, to_jsonb(enabled), created_at AS updated_at
            FROM feature_flags WHERE effective_to IS NULL
        ) source
    LOOP
        CONTINUE WHEN r.rn > 1;

        inf_type := _cff000005_infer_value_type(r.value);

        INSERT INTO config_definitions (
            definition_id, key, owner, value_type, safety_class, allowed_scopes,
            default_value, fallback_policy, sensitivity, validation, effective_model,
            lifecycle, deprecation, flag_class, retirement_deadline,
            created_by_principal_id, updated_by_principal_id
        )
        VALUES (
            gen_random_uuid(), r.key, 'platform-operations', inf_type, 'S1',
            '["ENVIRONMENT","TENANT"]'::JSONB,
            NULL, 'BLOCK', 'INTERNAL', NULL, 'IMMEDIATE',
            'PUBLISHED', NULL, NULL, NULL,
            'system:migration-000005', 'system:migration-000005'
        )
        ON CONFLICT (key) DO NOTHING
        RETURNING definition_id INTO def_id;

        IF def_id IS NULL THEN
            SELECT definition_id INTO def_id FROM config_definitions WHERE key = r.key;
        END IF;

        def_json := jsonb_build_object(
            'key',              r.key,
            'owner',            'platform-operations',
            'value_type',       inf_type,
            'safety_class',     'S1',
            'allowed_scopes',   '["ENVIRONMENT","TENANT"]'::JSONB,
            'default_value',    NULL,
            'fallback_policy',  'BLOCK',
            'sensitivity',      'INTERNAL',
            'validation',       NULL,
            'effective_model',  'IMMEDIATE',
            'lifecycle',        'PUBLISHED',
            'deprecation',      NULL,
            'flag_class',       NULL,
            'retirement_deadline', NULL
        );
        v_digest := md5(def_json::text);

        INSERT INTO config_definition_versions
            (definition_id, version, digest, definition, lifecycle, published_by_principal_id)
        VALUES
            (def_id, 1, v_digest, def_json, 'PUBLISHED', 'system:migration-000005')
        ON CONFLICT (definition_id, version) DO NOTHING;
    END LOOP;
END
$backfill$;

DROP FUNCTION IF EXISTS _cff000005_infer_value_type(JSONB);