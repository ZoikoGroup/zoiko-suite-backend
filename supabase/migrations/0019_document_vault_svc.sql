-- 0019_document_vault_svc.sql
-- document-vault-svc → schema `document_vault`
--
-- Squashed end state of 000001_initial_schema and
-- 000002_force_rls_and_invariants. Three tables: documents, document_versions,
-- document_access_log.
--
-- "ZoikoSuite does not merely store documents. It preserves documentary
-- evidence as part of operational truth" — hence the strict split: `documents`
-- holds the current pointer and metadata, `document_versions` is an append-only
-- lineage (a new version is always a new row), and `document_access_log` is an
-- append-only record of every read.
--
-- ── The bytes are not in here ────────────────────────────────────────────────
-- document_versions.storage_key is a POINTER. On the compose estate it names an
-- object in the service's own storage; on Supabase the natural home is Supabase
-- Storage, and the key becomes the object path in a bucket. This migration
-- creates no bucket and takes no position on which — it only preserves the
-- column and the checksum that validates whatever it points at.
--
-- ── What 000002 had to correct, and is simply correct here ───────────────────
-- These three tables had NO row security at all — no policy, and nothing that
-- would fail closed if the application's own tenant predicate were wrong or
-- absent. It WAS absent: the store's predicate read
-- `($2::uuid IS NULL OR tenant_id = $2::uuid)`, which disables itself when the
-- request carries no X-Tenant-Id. On a vault holding CONFIDENTIAL and
-- RESTRICTED documents there was no tenant boundary of any kind for a caller
-- who simply omitted the header.
--
-- The CHECK constraints are VALID here rather than NOT VALID. 000002 had to
-- preserve rows recording 'unknown' as the accessing principal — the true
-- record of what the service did, which a migration must not rewrite. This
-- database has no such backlog.

CREATE SCHEMA IF NOT EXISTS document_vault;

COMMENT ON SCHEMA document_vault IS
    'document-vault-svc. Governed documents with append-only version lineage and access history. Bytes live in object storage; storage_key points at them.';

GRANT USAGE ON SCHEMA document_vault TO zoiko_backend, authenticated;

-- ── documents ────────────────────────────────────────────────────────────────

CREATE TABLE document_vault.documents (
    document_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID         NOT NULL,
    legal_entity_id         UUID         NOT NULL,

    title                   VARCHAR(500) NOT NULL,

    classification          VARCHAR(20)  NOT NULL
        CHECK (classification IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED')),

    -- A named policy code, NOT a retention engine. Nothing enforces it: no
    -- purge is scheduled by it, no delete is blocked by it, and there is no
    -- delete route at all. ErrRetentionActive is defined in the domain and
    -- never returned. A document marked for a seven-year hold is not held by
    -- anything in this vault — the label records an intention some other system
    -- would have to honour. Recorded here so the column is not over-read.
    retention_policy        VARCHAR(50)  NOT NULL DEFAULT 'DEFAULT',

    -- Jurisdiction-aware residency. Nullable: not every document is
    -- residency-constrained. A region code, not a cross-service foreign key.
    residency_region_code   VARCHAR(20),

    current_version         INT          NOT NULL DEFAULT 1,

    status                  VARCHAR(20)  NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'RETAINED', 'PURGE_PENDING')),

    created_by_principal_id VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- The handler used to substitute the literal 'unknown' when a request
    -- carried no identity, so a record meant to say who created a document
    -- could say nothing and read as though it had said something.
    CONSTRAINT documents_created_by_identified
        CHECK (created_by_principal_id <> '' AND created_by_principal_id <> 'unknown'),

    -- A document claiming version 0 or a negative one points at a version row
    -- that cannot exist.
    CONSTRAINT documents_current_version_positive
        CHECK (current_version >= 1)
);

CREATE INDEX idx_documents_tenant
    ON document_vault.documents (tenant_id, legal_entity_id);
CREATE INDEX idx_documents_tenant_created
    ON document_vault.documents (tenant_id, created_at DESC, document_id DESC);

-- ── document_versions ────────────────────────────────────────────────────────
-- Append-only. A document "update" NEVER updates this table — it inserts a new
-- row and bumps documents.current_version. Integrity validation is the SHA-256
-- checksum, computed on write and re-checked on every read.

CREATE TABLE document_vault.document_versions (
    document_version_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id             UUID         NOT NULL REFERENCES document_vault.documents(document_id),

    version                 INT          NOT NULL,
    checksum_sha256         VARCHAR(64)  NOT NULL,

    -- Pointer to the stored object, not the bytes themselves.
    storage_key             VARCHAR(500) NOT NULL,

    size_bytes              BIGINT       NOT NULL,
    content_type            VARCHAR(255) NOT NULL,

    created_by_principal_id VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),

    UNIQUE (document_id, version),

    CONSTRAINT document_versions_created_by_identified
        CHECK (created_by_principal_id <> '' AND created_by_principal_id <> 'unknown'),
    CONSTRAINT document_versions_version_positive
        CHECK (version >= 1),

    -- A checksum IS the integrity control, recomputed on every read. A row with
    -- a blank or short one has no integrity control at all.
    CONSTRAINT document_versions_checksum_present
        CHECK (length(checksum_sha256) = 64),
    CONSTRAINT document_versions_size_nonnegative
        CHECK (size_bytes >= 0)
);

CREATE INDEX idx_document_versions_document
    ON document_vault.document_versions (document_id, version);

CREATE TRIGGER document_versions_immutable
    BEFORE UPDATE OR DELETE ON document_vault.document_versions
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── document_access_log ──────────────────────────────────────────────────────
-- Append-only record of every read.
--
-- OPEN GAP, carried over unchanged: GET /{id} and /{id}/content append a row
-- here; the register LIST route does not. So the log is a complete account of
-- DOWNLOADS and single-document reads only, and understates who has seen
-- metadata. Recording one row per document per list call would make the log
-- unreadable for its actual purpose, and a "LIST" access type covering a result
-- set is a schema decision rather than a bug fix — so access_type keeps its two
-- values here and the question stays open.

CREATE TABLE document_vault.document_access_log (
    access_log_id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id              UUID         NOT NULL REFERENCES document_vault.documents(document_id),
    document_version_id      UUID         REFERENCES document_vault.document_versions(document_version_id),

    accessed_by_principal_id VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),

    access_type              VARCHAR(20)  NOT NULL
        CHECK (access_type IN ('METADATA', 'DOWNLOAD')),

    correlation_id           VARCHAR(255),
    accessed_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- The log was accepting the literal string 'unknown' as a principal,
    -- because the handler substituted that when a request carried no identity.
    -- So the append-only record that exists to answer "who downloaded this
    -- RESTRICTED document" could answer "unknown", and read as though it had
    -- answered.
    CONSTRAINT document_access_log_principal_identified
        CHECK (accessed_by_principal_id <> '' AND accessed_by_principal_id <> 'unknown')
);

CREATE INDEX idx_document_access_log_document
    ON document_vault.document_access_log (document_id, accessed_at);

-- Read newest-first and paged; accessed_at alone is not a total order, so the
-- primary key rides along as the tiebreaker.
CREATE INDEX idx_document_access_log_document_paged
    ON document_vault.document_access_log (document_id, accessed_at DESC, access_log_id DESC);

CREATE TRIGGER document_access_log_immutable
    BEFORE UPDATE OR DELETE ON document_vault.document_access_log
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────
-- document_versions and document_access_log carry no tenant_id of their own —
-- they hang off documents. Scoping them through that parent rather than
-- denormalising a tenant column keeps ONE source of truth for which tenant owns
-- a document, and means a version or an access-log row can never disagree with
-- its document about it.
--
-- The EXISTS is itself subject to the documents policy, so a version whose
-- parent is invisible to this tenant is invisible too.

ALTER TABLE document_vault.documents           ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_vault.documents           FORCE  ROW LEVEL SECURITY;
ALTER TABLE document_vault.document_versions   ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_vault.document_versions   FORCE  ROW LEVEL SECURITY;
ALTER TABLE document_vault.document_access_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_vault.document_access_log FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON document_vault.documents
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON document_vault.documents
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_isolation ON document_vault.document_versions
    FOR ALL
    TO zoiko_backend
    USING      (EXISTS (SELECT 1 FROM document_vault.documents d
                         WHERE d.document_id = document_versions.document_id))
    WITH CHECK (EXISTS (SELECT 1 FROM document_vault.documents d
                         WHERE d.document_id = document_versions.document_id));

CREATE POLICY tenant_read ON document_vault.document_versions
    FOR SELECT
    TO authenticated
    USING (EXISTS (SELECT 1 FROM document_vault.documents d
                    WHERE d.document_id = document_versions.document_id));

CREATE POLICY tenant_isolation ON document_vault.document_access_log
    FOR ALL
    TO zoiko_backend
    USING      (EXISTS (SELECT 1 FROM document_vault.documents d
                         WHERE d.document_id = document_access_log.document_id))
    WITH CHECK (EXISTS (SELECT 1 FROM document_vault.documents d
                         WHERE d.document_id = document_access_log.document_id));

CREATE POLICY tenant_read ON document_vault.document_access_log
    FOR SELECT
    TO authenticated
    USING (EXISTS (SELECT 1 FROM document_vault.documents d
                    WHERE d.document_id = document_access_log.document_id));

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON document_vault.documents           TO authenticated;
GRANT SELECT ON document_vault.document_versions   TO authenticated;
GRANT SELECT ON document_vault.document_access_log TO authenticated;

-- documents transitions (status, current_version), so UPDATE is granted. The
-- lineage and the access log never change.
GRANT SELECT, INSERT, UPDATE ON document_vault.documents           TO zoiko_backend;
GRANT SELECT, INSERT         ON document_vault.document_versions   TO zoiko_backend;
GRANT SELECT, INSERT         ON document_vault.document_access_log TO zoiko_backend;
