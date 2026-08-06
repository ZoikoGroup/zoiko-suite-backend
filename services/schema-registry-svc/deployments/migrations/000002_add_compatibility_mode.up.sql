-- 000002_add_compatibility_mode.up.sql
--
-- docs/architecture/04-data-model.md §17.2 states plainly: "compatibility mode
-- must be declared". It was not. Every schema registered through this service
-- was checked for backward compatibility whatever its author intended, and
-- nothing recorded which discipline a given event contract is held to.
--
-- That produced two wrong behaviours in opposite directions. A contract that
-- genuinely is allowed a breaking change — an internal event with no external
-- consumers yet, during a controlled rollout (§17.2) — could not be evolved at
-- all except by registering it under a new name. And a reader of the registry
-- had no way to tell whether a schema's compatibility had been considered or
-- merely defaulted.
--
-- §17.1 also lists owning_service on SchemaRegistryArtifact. Without it the
-- registry can say a contract changed but not who is responsible for it,
-- which is the first question asked when one breaks.
--
-- VARCHAR rather than an enum, per the same doctrine the rest of the platform
-- follows: new modes arrive by data migration, not a code change.
--
--   BACKWARD — a new version must not break existing consumers. The default,
--              and what the service previously applied unconditionally.
--   NONE     — evolution is unchecked. Deliberately available for contracts
--              under controlled rollout; the mode is recorded on the row so
--              the exemption is visible rather than assumed.

ALTER TABLE event_schemas
    ADD COLUMN IF NOT EXISTS compatibility_mode VARCHAR(32) NOT NULL DEFAULT 'BACKWARD';

ALTER TABLE event_schemas
    ADD COLUMN IF NOT EXISTS owning_service VARCHAR(255);

-- Existing rows were all registered under the hardcoded backward check, so the
-- DEFAULT above is the truthful value for them — no backfill is needed.

-- Supports "which contracts are exempt from compatibility checking", the
-- question a governance reader asks of this table.
CREATE INDEX IF NOT EXISTS idx_event_schemas_compatibility_mode
    ON event_schemas (compatibility_mode)
    WHERE compatibility_mode <> 'BACKWARD';
