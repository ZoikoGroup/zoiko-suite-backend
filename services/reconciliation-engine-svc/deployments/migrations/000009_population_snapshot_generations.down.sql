-- Migration: 000009_population_snapshot_generations.down.sql
--
-- Reverses 000009. Once a run holds more than one generation of a side, the
-- old one-per-side shape cannot express what is stored, so the down refuses
-- rather than dropping a re-read's evidence. (Same posture as 000006's down,
-- which refuses while live history exists.)

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM population_snapshots GROUP BY run_id, side HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot remove snapshot generations while a run holds more than one generation of a side';
    END IF;
END $$;

ALTER TABLE population_snapshots
    DROP CONSTRAINT IF EXISTS population_snapshots_run_id_side_generation_key;

ALTER TABLE population_snapshots
    DROP COLUMN IF EXISTS generation;

ALTER TABLE population_snapshots
    ADD CONSTRAINT population_snapshots_run_id_side_key UNIQUE (run_id, side);
