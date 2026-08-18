-- Backstops for the invariants this registry depends on but never stated.
--
-- Each is added NOT VALID: the constraint applies to every INSERT and UPDATE
-- from here on, but the scan that would reject the table outright if an
-- existing row violates it is skipped. This table is append-only by doctrine —
-- a schema version is never edited or deleted once registered — so rows
-- written before these rules existed are history, not errors to be corrected
-- by a migration. Run ALTER TABLE ... VALIDATE CONSTRAINT once someone has
-- looked at whatever the backlog contains.

-- json_schema is JSONB, so Postgres already guaranteed well-formed JSON — and
-- nothing more. `123`, `"a string"`, `null` and `[]` are all valid JSONB, and
-- the service's own check was json.Valid, which accepts every one of them.
--
-- A first version stored as `123` cannot be evolved: the next registration
-- runs the compatibility check, the stored baseline fails to parse into a
-- shape, and every future version of that event answers 400 forever. The
-- registry accepted a value that permanently bricked the contract it recorded.
-- The service now refuses non-objects at the boundary; this is the backstop
-- for anything writing to the table without going through it.
ALTER TABLE event_schemas
    ADD CONSTRAINT event_schemas_json_schema_is_object
    CHECK (jsonb_typeof(json_schema) = 'object') NOT VALID;

-- `{}` is an object that constrains nothing. A contract permitting every
-- payload is not a contract, and recording one lets a producer claim its
-- events are governed when nothing about them is specified.
ALTER TABLE event_schemas
    ADD CONSTRAINT event_schemas_json_schema_not_empty
    CHECK (json_schema <> '{}'::jsonb) NOT VALID;

-- Versions are assigned by the registry and start at 1. Nothing stopped a 0 or
-- a negative one being written directly.
ALTER TABLE event_schemas
    ADD CONSTRAINT event_schemas_version_positive
    CHECK (version >= 1) NOT VALID;

-- compatibility_mode is the discipline a version was accepted under. The
-- service refuses any mode it cannot enforce rather than defaulting it, so the
-- column should only ever hold one of these two; new modes arrive by data
-- migration, which is exactly when this constraint should be updated.
ALTER TABLE event_schemas
    ADD CONSTRAINT event_schemas_compatibility_mode_known
    CHECK (compatibility_mode IN ('BACKWARD', 'NONE')) NOT VALID;

-- The event name is this registry's primary key and was a free-text field: the
-- service accepted any non-empty string. The convention every publisher on the
-- platform already follows is dotted lowercase with at least two segments
-- (jurisdiction.rule.updated), and the service now enforces it.
ALTER TABLE event_schemas
    ADD CONSTRAINT event_schemas_event_name_wellformed
    CHECK (event_name ~ '^[a-z][a-z0-9]*(\.[a-z0-9]+)+$') NOT VALID;

-- The register is read newest-name-first and paged; the existing index covers
-- lookups by name but not the DISTINCT ordering the catalogue read performs.
CREATE INDEX IF NOT EXISTS idx_event_schemas_owning_service
    ON event_schemas (owning_service)
    WHERE owning_service IS NOT NULL;
