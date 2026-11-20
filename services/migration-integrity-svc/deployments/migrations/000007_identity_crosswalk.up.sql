-- Migration: 000007_identity_crosswalk.up.sql
--
-- ZS-MIG-001 MIG-INV-14: "legacy and canonical identifiers remain mutually
-- discoverable" -- permanently, across every extract and batch that ever
-- touches the same source record, not just for the lifetime of one
-- migration_jobs row. A (source_system, source_type, source_id) triple maps
-- to exactly one canonical_id for as long as this table exists: later
-- extracts look an old source_id up here instead of being asked to decide
-- its canonical identity again, and an auditor can walk from either
-- direction.
CREATE TABLE IF NOT EXISTS migration_identity_crosswalk (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,

    source_system VARCHAR(128) NOT NULL,
    source_type VARCHAR(128) NOT NULL,  -- e.g. ACCOUNT, CUSTOMER, VENDOR, GL_LINE
    source_id VARCHAR(256) NOT NULL,

    canonical_id VARCHAR(256) NOT NULL, -- the platform's own identifier for the same real-world object

    -- Match evidence: how this correspondence was established (MIG-INV-12
    -- style -- a decision needs a reason, not just an outcome). match_method
    -- is e.g. EXACT, DETERMINISTIC_RULE, MANUAL, FUZZY; match_evidence is the
    -- free-text/JSON support for it (rule name, confidence, source extract
    -- reference).
    match_method VARCHAR(32) NOT NULL,
    match_evidence TEXT,
    matched_by VARCHAR(256) NOT NULL,

    -- The job that FIRST established this mapping. Kept for provenance; the
    -- mapping itself outlives the job (MIG-INV-14 is "permanently", a job is
    -- one run).
    first_job_id UUID REFERENCES migration_jobs(id),

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The identity this service exists to keep stable: the same legacy key
    -- resolves to the same canonical_id every time, across every batch.
    CONSTRAINT migration_crosswalk_source_unique
        UNIQUE (tenant_id, source_system, source_type, source_id)
);

ALTER TABLE migration_identity_crosswalk ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS migration_identity_crosswalk_isolation ON migration_identity_crosswalk;
CREATE POLICY migration_identity_crosswalk_isolation ON migration_identity_crosswalk
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Discoverable from the canonical side too (MIG-INV-14 "mutually").
CREATE INDEX IF NOT EXISTS idx_migration_crosswalk_canonical
    ON migration_identity_crosswalk(tenant_id, canonical_id);
