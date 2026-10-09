-- 000006_add_missing_schema_fields.up.sql
-- Policy Service — add missing fields per 04-data-model.md §7.1 Policy/PolicyVersion
--
-- Adds fields required by the canonical data model:
--   policies: tenant_id, policy_status, versioning_mode
--   policy_versions: version_number, source, rationale, artifact_digest, known_from

-- ── policies ─────────────────────────────────────────────────────────────────

-- tenant_id: nullable, NULL means platform-wide policy family (not tenant-owned)
-- Per 04-data-model.md §7.1: Policy is tenant-owned, but platform-wide policies exist.
-- A policy with tenant_id=NULL is a platform-wide policy family.
ALTER TABLE policies
    ADD COLUMN tenant_id UUID;

-- policy_status: DRAFT | ACTIVE | RETIRED (policy-level lifecycle, not version-level)
-- Per 04-data-model.md §7.1: policy_status tracks the policy family's lifecycle.
ALTER TABLE policies
    ADD COLUMN policy_status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE';

-- versioning_mode: SIMPLE | BRANCHED | FORMAL — controls how versions are created/managed
-- Per 04-data-model.md §7.1: versioning_mode controls the versioning strategy.
ALTER TABLE policies
    ADD COLUMN versioning_mode VARCHAR(32) NOT NULL DEFAULT 'SIMPLE';

-- Index for tenant-scoped policy lookups
CREATE INDEX idx_policies_tenant ON policies (tenant_id);

-- ── policy_versions ──────────────────────────────────────────────────────────

-- version_number: sequential integer per policy, for human readability
-- Per 04-data-model.md §7.1: version_number is a sequential identifier.
ALTER TABLE policy_versions
    ADD COLUMN version_number INTEGER;

-- source: where this version originated (e.g. "internal", "imported", "migrated")
-- Per GCP §18 / V-001 §7: source tracks provenance.
ALTER TABLE policy_versions
    ADD COLUMN source TEXT NOT NULL DEFAULT 'internal';

-- rationale: human-readable explanation for why this version was created
-- Per GCP §18 / V-001 §7: rationale documents the intent.
ALTER TABLE policy_versions
    ADD COLUMN rationale TEXT;

-- artifact_digest: SHA256 of the rule_payload for integrity verification
-- Per GCP §18 / V-001 §8.1: artifact_digest enables exact reproducibility.
ALTER TABLE policy_versions
    ADD COLUMN artifact_digest TEXT;

-- known_from: when this version became known to the platform (decision_as_of / known_at)
-- Per V-001 §8.1: known_from supports bitemporal queries.
ALTER TABLE policy_versions
    ADD COLUMN known_from TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Update the dedup unique index to include version_number for disambiguation
-- (policy_id, tenant_id, legal_entity_id, effective_from, version_number)
DROP INDEX IF EXISTS idx_policy_versions_dedup;
CREATE UNIQUE INDEX idx_policy_versions_dedup ON policy_versions (
    policy_id,
    COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
    COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
    effective_from,
    COALESCE(version_number, 0)
);

-- Backfill version_number for existing versions (sequential per policy+effective_from)
-- This is a best-effort backfill; exact ordering depends on created_at
WITH numbered AS (
    SELECT
        policy_version_id,
        ROW_NUMBER() OVER (
            PARTITION BY policy_id, COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
                          COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
                          effective_from
            ORDER BY created_at
        ) AS vn
    FROM policy_versions
)
UPDATE policy_versions pv
SET version_number = n.vn
FROM numbered n
WHERE pv.policy_version_id = n.policy_version_id;

-- Populate artifact_digest for existing versions
UPDATE policy_versions
SET artifact_digest = 'sha256-' || encode(sha256(rule_payload::text), 'hex')
WHERE artifact_digest IS NULL;