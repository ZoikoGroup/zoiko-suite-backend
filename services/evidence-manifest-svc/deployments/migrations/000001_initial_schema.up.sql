-- Evidence Manifest Service — initial schema.
--
-- Per docs/architecture/03-microservices.md §14.4: builds structured evidence
-- sets for audit/regulator/legal-discovery/compliance-review scenarios. Per
-- the platform-wide evidential doctrine (01-backend.md, "Required Properties
-- of Every Evidential Record"): a manifest, once generated, is never edited —
-- a re-run produces a new manifest and new records, never mutates old ones.

CREATE TABLE evidence_manifests (
    manifest_id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    legal_entity_id   UUID NOT NULL,
    scenario_type     VARCHAR(30) NOT NULL
        CHECK (scenario_type IN ('AUDIT', 'REGULATOR', 'LEGAL_DISCOVERY', 'COMPLIANCE_REVIEW')),
    requested_by      VARCHAR(255) NOT NULL,
    status            VARCHAR(20) NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'GENERATED', 'FAILED')),
    checksum_sha256   VARCHAR(64),
    failure_reason    TEXT,
    requested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    generated_at      TIMESTAMPTZ
);

CREATE INDEX idx_evidence_manifests_tenant ON evidence_manifests (tenant_id, legal_entity_id);

-- Append-only: one row per source record pulled into a manifest, with a full
-- JSON snapshot of that record as it existed at generation time — so the
-- manifest stays retrievable and reconstructable even if the source service
-- is later unavailable or the source record has since changed.
CREATE TABLE manifest_records (
    manifest_record_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    manifest_id         UUID NOT NULL REFERENCES evidence_manifests(manifest_id),
    source_type         VARCHAR(30) NOT NULL
        CHECK (source_type IN ('GOVERNANCE_DECISION', 'ACCESS_DECISION', 'WORKFLOW_INSTANCE')),
    source_record_id    VARCHAR(255) NOT NULL,
    record_snapshot     JSONB NOT NULL,
    fetched_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_manifest_records_manifest ON manifest_records (manifest_id);
