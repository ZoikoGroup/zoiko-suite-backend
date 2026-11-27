-- ZS-SVC-Z-001 DQC-05 / TC-13 / section 11 (docs/architecture/DQC_GAP_MATRIX.md, step C).
--
-- A certification used to have no identity independent of its run, never went
-- stale, and rested on no stated lineage. Three things change, all additive:
--
--   1. basis_sha256 hashes WHAT was certified (the pinned rules, the frozen
--      populations by content, the result, the lineage reference) and deliberately
--      NOT the run, snapshot ids, certifier or any timestamp. The same evidence
--      has the same basis wherever and whenever it is sealed, and it is unique per
--      tenant: a second certification of an identical basis is refused.
--   2. A certification can name the lineage manifest it rests on (id + sha256),
--      bound into the basis. It is a reference into data-lineage-svc, recorded
--      and not re-verified here.
--   3. The freshness state it was issued under is sealed with it: the oldest
--      frozen population's time and the window the profile held it to (the
--      window itself lives in the pinned certification profile, not here).
--
-- All new columns are nullable. Certifications sealed before this migration have
-- no basis and cannot be given one honestly, so they are left exactly as they
-- were: nothing is backfilled and nothing is rewritten.

ALTER TABLE certifications
    ADD COLUMN IF NOT EXISTS basis_sha256              CHAR(64),
    ADD COLUMN IF NOT EXISTS lineage_manifest_id       TEXT,
    ADD COLUMN IF NOT EXISTS lineage_manifest_sha256   CHAR(64),
    ADD COLUMN IF NOT EXISTS evidence_oldest_at        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS evidence_max_age_seconds  BIGINT;

ALTER TABLE certifications
    ADD CONSTRAINT certifications_basis_format
        CHECK (basis_sha256 IS NULL OR basis_sha256 ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT certifications_lineage_ref_format
        CHECK (
            (lineage_manifest_id IS NULL AND lineage_manifest_sha256 IS NULL)
            OR (lineage_manifest_id ~ '^dpm_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                AND lineage_manifest_sha256 ~ '^[0-9a-f]{64}$')
        ),
    -- A new-style certification states the evidence time its freshness was
    -- judged from; a basis with none would read as evidenced while proving nothing.
    ADD CONSTRAINT certifications_basis_is_complete
        CHECK (basis_sha256 IS NULL OR evidence_oldest_at IS NOT NULL),
    ADD CONSTRAINT certifications_max_age_positive
        CHECK (evidence_max_age_seconds IS NULL OR evidence_max_age_seconds > 0);

-- section 11: "unique certification basis hash".
CREATE UNIQUE INDEX IF NOT EXISTS uq_certifications_basis
    ON certifications (tenant_id, basis_sha256)
    WHERE basis_sha256 IS NOT NULL;
