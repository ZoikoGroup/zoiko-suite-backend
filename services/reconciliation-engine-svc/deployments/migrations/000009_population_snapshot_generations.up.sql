-- Migration: 000009_population_snapshot_generations.up.sql
--
-- A8  Reperformance re-reads the run's populations. A run's snapshots were
--     one-per-side (UNIQUE (run_id, side)); a re-read needs to freeze what it
--     pulled WITHOUT editing the original — the "frozen populations" control
--     is per generation, and the earlier generation stays immutable beside the
--     new one. Generation 1 is the run's initial freeze (StartRun); each
--     reperformance freezes the next generation, and matching, balance checks
--     and certification totals read the latest generation only
--     (population_snapshots.generation = MAX(generation) for the run).
--
-- Existing snapshots become generation 1 unchanged.

ALTER TABLE population_snapshots
    DROP CONSTRAINT IF EXISTS population_snapshots_run_id_side_key;

ALTER TABLE population_snapshots
    ADD COLUMN IF NOT EXISTS generation INTEGER NOT NULL DEFAULT 1 CHECK (generation >= 1);

-- One snapshot per run, side AND generation; re-reads add generations, never rows.
ALTER TABLE population_snapshots
    ADD CONSTRAINT population_snapshots_run_id_side_generation_key UNIQUE (run_id, side, generation);
