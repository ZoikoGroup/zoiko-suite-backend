-- Migration: 000004_snapshots.down.sql
--
-- Removes the snapshot apparatus (epoch counter, imprints, manifest
-- function) and restores config_entries / feature_flags to the exact
-- tenant_isolation_policy 000002 created — the request-path isolation this
-- migration only ever added a mint escape to. Dropping the escape but
-- leaving the tables would strand a FORCE-RLS write path with no way to
-- write, so the tables go too.

DROP POLICY IF EXISTS snapshots_mint_only ON config_snapshots;
DROP POLICY IF EXISTS snapshot_epochs_mint_only ON config_snapshot_epochs;

DROP FUNCTION IF EXISTS config_environment_manifest(TEXT);

DROP TABLE IF EXISTS config_snapshots;
DROP TABLE IF EXISTS config_snapshot_epochs;

-- Restore the original isolation policy, verbatim from 000002 — no
-- app.snapshot_mint disjunct.
DROP POLICY IF EXISTS tenant_isolation_policy ON feature_flags;
CREATE POLICY tenant_isolation_policy ON feature_flags
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON config_entries;
CREATE POLICY tenant_isolation_policy ON config_entries
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );