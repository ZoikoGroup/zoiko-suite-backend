-- ZS-SVC-Z-001 INV-24 (docs/architecture/DQC_GAP_MATRIX.md, step D).
--
-- "Control-total hashes pin canonical serialization and algorithm version."
-- population_snapshots.content_hash was a bare CHAR(64): nothing said how it was
-- made, so a hash could only be trusted, never re-derived, and any future change
-- to the format would silently invalidate every old hash. Each snapshot now
-- records the algorithm and serialization it was hashed with, and a verifier can
-- re-derive it from the frozen items (VerifyPopulationHashes).
--
-- Every snapshot that exists today was hashed with exactly one format (the only
-- one there has ever been), so the backfill below is a statement of fact, not a
-- guess. The defaults are then DROPPED: a writer that forgets to say which format
-- it used must fail, not quietly claim the old one.

ALTER TABLE population_snapshots
    ADD COLUMN hash_algorithm     VARCHAR(16) NOT NULL DEFAULT 'sha256',
    ADD COLUMN hash_serialization VARCHAR(32) NOT NULL DEFAULT 'rpop-v1';

ALTER TABLE population_snapshots
    ALTER COLUMN hash_algorithm DROP DEFAULT,
    ALTER COLUMN hash_serialization DROP DEFAULT;

-- Widen these lists when a new format is introduced; never edit what an existing
-- value means.
ALTER TABLE population_snapshots
    ADD CONSTRAINT population_snapshots_hash_algorithm_check CHECK (hash_algorithm IN ('sha256')),
    ADD CONSTRAINT population_snapshots_hash_serialization_check CHECK (hash_serialization ~ '^rpop-v[0-9]+$');

-- A certification manifest is hashed after being encoded (Go encoding/json of the
-- manifest struct). Name that encoding. Nullable: certifications sealed before
-- this migration did not record it, and it cannot be honestly filled in for them.
ALTER TABLE certifications
    ADD COLUMN manifest_serialization VARCHAR(32)
        CHECK (manifest_serialization IS NULL OR manifest_serialization ~ '^rcm-v[0-9]+$');
