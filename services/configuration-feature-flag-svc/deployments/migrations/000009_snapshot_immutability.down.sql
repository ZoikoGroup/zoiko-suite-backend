DROP TRIGGER IF EXISTS trg_config_snapshots_no_truncate ON config_snapshots;
DROP TRIGGER IF EXISTS trg_config_snapshots_immutable ON config_snapshots;
DROP FUNCTION IF EXISTS config_snapshot_immutable();
