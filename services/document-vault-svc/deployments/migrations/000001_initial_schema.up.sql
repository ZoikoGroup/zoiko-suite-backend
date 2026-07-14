-- Document Vault Service — initial schema.
--
-- Per docs/architecture/01-backend.md §8.3: documents are stored with version
-- history, access history, approval lineage, integrity validation, retention
-- policies, and jurisdiction-aware residency controls. "ZoikoSuite does not
-- merely store documents. It preserves documentary evidence as part of
-- operational truth" — hence the strict split below: `documents` holds the
-- current pointer/metadata, `document_versions` is an append-only lineage
-- (never UPDATE/DELETE a row — a new version is always a new row), and
-- `document_access_log` is an append-only record of every read (doctrine
-- shared with authorization-svc's access_decision_log).

CREATE TABLE documents (
    document_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL,
    legal_entity_id         UUID NOT NULL,
    title                   VARCHAR(500) NOT NULL,
    -- Data classification baseline (04-data-model.md §20.1).
    classification          VARCHAR(20) NOT NULL
        CHECK (classification IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED')),
    -- Simple v1 retention model: a named policy code, not a full retention
    -- engine — enforcement (reject premature delete) reads this string.
    retention_policy        VARCHAR(50) NOT NULL DEFAULT 'DEFAULT',
    -- Jurisdiction-aware residency (§8.3). Nullable: not every document is
    -- residency-constrained. Region code, not a cross-service FK — this
    -- service does not share a database with tenant-entity-registry-svc.
    residency_region_code   VARCHAR(20),
    current_version         INT NOT NULL DEFAULT 1,
    status                  VARCHAR(20) NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'RETAINED', 'PURGE_PENDING')),
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_documents_tenant ON documents (tenant_id, legal_entity_id);

-- Append-only version lineage. A document "update" NEVER updates this table —
-- it inserts a new row and bumps documents.current_version. Integrity
-- validation (§8.3) is the SHA-256 checksum, computed on write and re-checked
-- on every read (internal/storage).
CREATE TABLE document_versions (
    document_version_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id             UUID NOT NULL REFERENCES documents(document_id),
    version                 INT NOT NULL,
    checksum_sha256         VARCHAR(64) NOT NULL,
    storage_key             VARCHAR(500) NOT NULL,
    size_bytes              BIGINT NOT NULL,
    content_type            VARCHAR(255) NOT NULL,
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (document_id, version)
);

CREATE INDEX idx_document_versions_document ON document_versions (document_id, version);

-- Append-only access history (§8.3 "access history"). Every read of a
-- document's metadata or bytes appends a row here — never updated, never
-- deleted, same doctrine as authorization-svc's access_decision_log.
CREATE TABLE document_access_log (
    access_log_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id             UUID NOT NULL REFERENCES documents(document_id),
    document_version_id     UUID REFERENCES document_versions(document_version_id),
    accessed_by_principal_id VARCHAR(255) NOT NULL,
    access_type             VARCHAR(20) NOT NULL
        CHECK (access_type IN ('METADATA', 'DOWNLOAD')),
    correlation_id          VARCHAR(255),
    accessed_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_document_access_log_document ON document_access_log (document_id, accessed_at);
