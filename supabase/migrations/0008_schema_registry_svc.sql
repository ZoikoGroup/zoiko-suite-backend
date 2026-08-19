-- 0008_schema_registry_svc.sql
-- schema-registry-svc → schema `schema_registry`
--
-- Squashed end state of 000001_initial_schema, 000002_add_compatibility_mode
-- and 000003_registry_invariants. One table: event_schemas.
--
-- ── No tenant dimension ──────────────────────────────────────────────────────
-- Like jurisdiction-rules, this is platform-wide reference data: event
-- contracts belong to the platform, not to a tenant. RLS is still enabled and
-- forced to keep the read/write asymmetry — readable by any authenticated
-- principal, writable only by the backend.
--
-- ── Append-only, enforced rather than asserted ───────────────────────────────
-- Doctrine: a schema version is never edited or deleted once registered.
-- Evolution always INSERTs a new (event_name, version) row. Here that is a
-- grant of SELECT and INSERT only, with no UPDATE or DELETE to anyone — the
-- compose estate stated the rule in a comment and left the table fully
-- writable.

CREATE SCHEMA IF NOT EXISTS schema_registry;

COMMENT ON SCHEMA schema_registry IS
    'schema-registry-svc. Append-only registry of event contract versions. Platform-wide, no tenant dimension.';

GRANT USAGE ON SCHEMA schema_registry TO zoiko_backend, authenticated;

-- ── event_schemas ────────────────────────────────────────────────────────────

CREATE TABLE schema_registry.event_schemas (
    event_name         VARCHAR(255) NOT NULL,
    version            INT          NOT NULL,
    json_schema        JSONB        NOT NULL,

    -- The discipline this version was accepted under. VARCHAR not enum, per
    -- platform doctrine: new modes arrive by data migration, not code change.
    --
    --   BACKWARD — a new version must not break existing consumers. The
    --              default, and what the service once applied unconditionally
    --              to everything whatever its author intended.
    --   NONE     — evolution unchecked. Deliberately available for a contract
    --              under controlled rollout; recording the mode on the row
    --              makes the exemption visible rather than assumed.
    compatibility_mode VARCHAR(32)  NOT NULL DEFAULT 'BACKWARD',

    -- Without this the registry can say a contract changed but not who is
    -- responsible for it — the first question asked when one breaks.
    owning_service     VARCHAR(255),

    registered_by      VARCHAR(255) DEFAULT app.current_principal_id(),
    registered_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    PRIMARY KEY (event_name, version),

    -- json_schema is JSONB, so Postgres guarantees well-formed JSON — and
    -- nothing more. `123`, `"a string"`, `null` and `[]` are all valid JSONB,
    -- and the service's own check was json.Valid, which accepts every one.
    --
    -- A first version stored as `123` cannot be evolved: the next registration
    -- runs the compatibility check, the stored baseline fails to parse into a
    -- shape, and every future version of that event answers 400 forever. The
    -- registry accepted a value that permanently bricked the contract it
    -- recorded.
    CONSTRAINT event_schemas_json_schema_is_object
        CHECK (jsonb_typeof(json_schema) = 'object'),

    -- `{}` is an object that constrains nothing. A contract permitting every
    -- payload is not a contract, and recording one lets a producer claim its
    -- events are governed when nothing about them is specified.
    CONSTRAINT event_schemas_json_schema_not_empty
        CHECK (json_schema <> '{}'::jsonb),

    -- Versions are assigned by the registry and start at 1.
    CONSTRAINT event_schemas_version_positive
        CHECK (version >= 1),

    CONSTRAINT event_schemas_compatibility_mode_known
        CHECK (compatibility_mode IN ('BACKWARD', 'NONE')),

    -- The event name is this registry's primary key and was free text: any
    -- non-empty string was accepted. The convention every publisher on the
    -- platform already follows is dotted lowercase with at least two segments
    -- (jurisdiction.rule.updated).
    CONSTRAINT event_schemas_event_name_wellformed
        CHECK (event_name ~ '^[a-z][a-z0-9]*(\.[a-z0-9]+)+$')
);

CREATE INDEX idx_event_schemas_event_name
    ON schema_registry.event_schemas (event_name);

-- Serves "which contracts are exempt from compatibility checking" — the
-- question a governance reader asks of this table.
CREATE INDEX idx_event_schemas_compatibility_mode
    ON schema_registry.event_schemas (compatibility_mode)
    WHERE compatibility_mode <> 'BACKWARD';

CREATE INDEX idx_event_schemas_owning_service
    ON schema_registry.event_schemas (owning_service)
    WHERE owning_service IS NOT NULL;

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE schema_registry.event_schemas ENABLE ROW LEVEL SECURITY;
ALTER TABLE schema_registry.event_schemas FORCE  ROW LEVEL SECURITY;

CREATE POLICY registry_read ON schema_registry.event_schemas
    FOR SELECT TO authenticated, zoiko_backend USING (true);

CREATE POLICY registry_append ON schema_registry.event_schemas
    FOR INSERT TO zoiko_backend WITH CHECK (true);

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON schema_registry.event_schemas TO authenticated;

-- SELECT and INSERT only. No UPDATE, no DELETE, to anyone — that is what
-- append-only means, and it is now enforced by the grant rather than asserted
-- in a comment.
GRANT SELECT, INSERT ON schema_registry.event_schemas TO zoiko_backend;
