-- 000009_snapshot_immutability
--
-- config_snapshots is the evidence base for INV-29: a consumer keeps the
-- snapshot_id an evaluation answered from, and reproducing that decision later
-- means fetching the same imprint and getting the same bytes. 000004 made the
-- table append-only by RLS alone, and RLS does not do that:
--
--   - snapshots_mint_only is FOR ALL USING (true). WITH CHECK constrains the
--     rows an INSERT or UPDATE produces; nothing constrains DELETE, so any
--     session could remove an imprint, and a session holding the mint flag
--     could rewrite one's content in place.
--   - The local stack connects as a superuser, which ignores FORCE RLS
--     entirely.
--
-- A trigger fires for every role, superuser included, so it is the guard
-- that actually holds. Nothing in the service updates or deletes a snapshot;
-- the store tests' teardown uses DROP TABLE, which no trigger intercepts.

CREATE OR REPLACE FUNCTION config_snapshot_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'config_snapshots is append-only: % refused (INV-29/INV-30)', TG_OP
        USING ERRCODE = 'P0001';
END
$$;

DROP TRIGGER IF EXISTS trg_config_snapshots_immutable ON config_snapshots;
CREATE TRIGGER trg_config_snapshots_immutable
    BEFORE UPDATE OR DELETE ON config_snapshots
    FOR EACH ROW EXECUTE FUNCTION config_snapshot_immutable();

DROP TRIGGER IF EXISTS trg_config_snapshots_no_truncate ON config_snapshots;
CREATE TRIGGER trg_config_snapshots_no_truncate
    BEFORE TRUNCATE ON config_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION config_snapshot_immutable();
