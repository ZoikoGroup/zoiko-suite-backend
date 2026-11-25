-- ZS-SVC-Z-001 DQC-05 / TC-13 / section 11 (docs/architecture/DQC_GAP_MATRIX.md, step C).
--
-- A certification used to bind a run and nothing about WHEN its evidence was
-- observed, and it had no identity independent of the run. Two things change:
--
--   1. Evidence freshness. A run can carry the time its population was observed
--      (caller-asserted, like the population hash); certification refuses
--      evidence older than the freshness window, and seals the age it was
--      certified at.
--   2. A certification basis. basis_sha256 hashes WHAT was certified (ruleset
--      version, population, every rule result, evidence time, window, lineage
--      reference) and deliberately NOT which run, who, or when it was sealed. The
--      same evidence therefore has the same basis, and is unique per tenant.
--
-- All new columns are nullable: certifications sealed before this migration have
-- no basis and cannot be given one honestly (their evidence-time was never
-- recorded), so they are left as they were. Nothing is backfilled.

ALTER TABLE dq_runs
    ADD COLUMN IF NOT EXISTS evidence_as_of TIMESTAMPTZ;

ALTER TABLE dq_certifications
    ADD COLUMN IF NOT EXISTS basis_sha256              CHAR(64),
    ADD COLUMN IF NOT EXISTS lineage_manifest_id       TEXT,
    ADD COLUMN IF NOT EXISTS lineage_manifest_sha256   CHAR(64),
    ADD COLUMN IF NOT EXISTS evidence_as_of            TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS evidence_max_age_seconds  BIGINT;

ALTER TABLE dq_certifications
    ADD CONSTRAINT dq_certifications_basis_format
        CHECK (basis_sha256 IS NULL OR basis_sha256 ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT dq_certifications_lineage_ref_format
        CHECK (
            (lineage_manifest_id IS NULL AND lineage_manifest_sha256 IS NULL)
            OR (lineage_manifest_id ~ '^dpm_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                AND lineage_manifest_sha256 ~ '^[0-9a-f]{64}$')
        ),
    -- A new-style certification states its basis and the window it was held to;
    -- a half-populated row would read as evidenced while proving nothing.
    ADD CONSTRAINT dq_certifications_basis_is_complete
        CHECK (
            (basis_sha256 IS NULL AND evidence_as_of IS NULL AND evidence_max_age_seconds IS NULL)
            OR (basis_sha256 IS NOT NULL AND evidence_as_of IS NOT NULL AND evidence_max_age_seconds IS NOT NULL
                AND evidence_max_age_seconds > 0)
        );

-- section 11: "unique certification basis hash". A second certification of the
-- identical basis is refused (the handler returns the existing one's id).
CREATE UNIQUE INDEX IF NOT EXISTS uq_dq_certifications_basis
    ON dq_certifications (tenant_id, basis_sha256)
    WHERE basis_sha256 IS NOT NULL;
