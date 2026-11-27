-- Reverting drops the recorded hash formats. The hashes themselves are untouched,
-- but they lose the statement of how they were made.
ALTER TABLE certifications DROP COLUMN IF EXISTS manifest_serialization;
ALTER TABLE population_snapshots
    DROP CONSTRAINT IF EXISTS population_snapshots_hash_serialization_check,
    DROP CONSTRAINT IF EXISTS population_snapshots_hash_algorithm_check;
ALTER TABLE population_snapshots DROP COLUMN IF EXISTS hash_serialization, DROP COLUMN IF EXISTS hash_algorithm;
