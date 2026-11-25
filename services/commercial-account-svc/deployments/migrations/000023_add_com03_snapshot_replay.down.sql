-- 000023_add_com03_snapshot_replay.down.sql

DROP INDEX IF EXISTS idx_entitlement_snapshots_batch;

ALTER TABLE entitlement_snapshots
    DROP COLUMN IF EXISTS snapshot_batch_id;
