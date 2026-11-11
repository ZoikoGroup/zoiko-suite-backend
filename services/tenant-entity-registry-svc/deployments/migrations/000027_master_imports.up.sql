-- 000027_master_imports.up.sql
--
-- ORG-9 master imports (ZS-SVC-U-001 s19: POST /v1/master-imports "staged/validated; row outcomes; no partial hidden
-- overwrites"; ZS-API-001 s18 async operations). An import is a file of party master records that moves through
-- upload -> validate -> preview -> approve -> import -> reconcile. Every row is kept with its own outcome and its own errors.
-- A file with any invalid row cannot be approved. Approval is by someone other than the uploader. Importing creates parties
-- through the ordinary party creation path, so a row that matches an existing party is a recorded conflict for stewardship,
-- never an overwrite. Reconciliation compares the rows received with the outcomes recorded and the import fails if they differ.
-- Nothing is deleted.

-- The shared API-5 operation table (identity-envelope apicontract/asyncpgx SchemaSQL and RLSSQL), service-owned.
CREATE TABLE IF NOT EXISTS async_operations (
    id                  text PRIMARY KEY,
    tenant_id           text NOT NULL,
    principal_id        text NOT NULL DEFAULT '',
    route               text NOT NULL DEFAULT '',
    type                text NOT NULL,
    state               text NOT NULL CHECK (state IN
        ('queued','running','waiting_external','succeeded','failed','cancel_requested','cancelled')),
    progress_completed  bigint NOT NULL DEFAULT 0 CHECK (progress_completed >= 0),
    progress_total      bigint NOT NULL DEFAULT 0 CHECK (progress_total >= 0),
    resource_ref        text NOT NULL DEFAULT '',
    problem             jsonb,
    cancelable          boolean NOT NULL DEFAULT true,
    idempotency_key     text NOT NULL DEFAULT '',
    request_hash        text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    started_at          timestamptz,
    finished_at         timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS async_operations_idempotency_idx
    ON async_operations (tenant_id, principal_id, route, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX IF NOT EXISTS async_operations_tenant_created_idx
    ON async_operations (tenant_id, created_at DESC);

ALTER TABLE async_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE async_operations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS async_operations_tenant_isolation ON async_operations;
CREATE POLICY async_operations_tenant_isolation ON async_operations
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE master_imports (
    import_id          UUID PRIMARY KEY,
    tenant_id          UUID NOT NULL REFERENCES tenants(tenant_id),
    kind               VARCHAR(16) NOT NULL,
    status             VARCHAR(16) NOT NULL,
    source_name        TEXT NOT NULL,
    source_version     TEXT NOT NULL,
    -- SHA-256 of the compacted rows as received, and the manifest hash the sender stated (checked on upload).
    content_sha256     VARCHAR(64) NOT NULL,
    manifest_sha256    VARCHAR(64),
    source_class       VARCHAR(24) NOT NULL,
    evidence_ref       TEXT NOT NULL,
    valid_from         DATE NOT NULL,
    row_count          INTEGER NOT NULL,
    valid_rows         INTEGER NOT NULL DEFAULT 0,
    invalid_rows       INTEGER NOT NULL DEFAULT 0,
    imported_rows      INTEGER NOT NULL DEFAULT 0,
    conflict_rows      INTEGER NOT NULL DEFAULT 0,
    failed_rows        INTEGER NOT NULL DEFAULT 0,
    -- The uploader's legal-entity scope, restored when the import runs on their behalf.
    scope_entity_id    UUID,
    uploaded_by        VARCHAR(255) NOT NULL,
    uploaded_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    validated_at       TIMESTAMP WITH TIME ZONE,
    approved_by        VARCHAR(255),
    approved_at        TIMESTAMP WITH TIME ZONE,
    approval_evidence  TEXT,
    started_at         TIMESTAMP WITH TIME ZONE,
    finished_at        TIMESTAMP WITH TIME ZONE,
    validate_op_id     TEXT,
    import_op_id       TEXT,
    reconciliation     JSONB,
    record_version     BIGINT NOT NULL DEFAULT 1,
    correlation_id     VARCHAR(255),
    CONSTRAINT mi_tenant_unique UNIQUE (tenant_id, import_id),
    CONSTRAINT mi_kind_known CHECK (kind IN ('PARTY')),
    CONSTRAINT mi_status_known CHECK (status IN ('UPLOADED', 'VALIDATING', 'PREVIEW_READY', 'BLOCKED', 'APPROVED', 'IMPORTING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    CONSTRAINT mi_hash_shape CHECK (content_sha256 ~ '^[0-9a-f]{64}$' AND (manifest_sha256 IS NULL OR manifest_sha256 ~ '^[0-9a-f]{64}$')),
    CONSTRAINT mi_rows_positive CHECK (row_count > 0 AND valid_rows >= 0 AND invalid_rows >= 0 AND imported_rows >= 0 AND conflict_rows >= 0 AND failed_rows >= 0),
    CONSTRAINT mi_present CHECK (btrim(source_name) <> '' AND btrim(source_version) <> '' AND btrim(evidence_ref) <> ''),
    -- A file with any invalid row is blocked: it is never approved, started or finished as an import.
    CONSTRAINT mi_invalid_blocks CHECK (invalid_rows = 0 OR status IN ('UPLOADED', 'VALIDATING', 'BLOCKED', 'CANCELLED', 'FAILED')),
    CONSTRAINT mi_approved_consistent CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
    CONSTRAINT mi_approved_when CHECK (status IN ('UPLOADED', 'VALIDATING', 'PREVIEW_READY', 'BLOCKED', 'CANCELLED', 'FAILED') OR approved_by IS NOT NULL),
    -- Whoever uploaded a file does not approve it.
    CONSTRAINT mi_independent CHECK (approved_by IS NULL OR approved_by <> uploaded_by),
    CONSTRAINT mi_version_positive CHECK (record_version > 0)
);
CREATE INDEX idx_mi_tenant ON master_imports (tenant_id, uploaded_at DESC);

CREATE TABLE master_import_rows (
    import_id    UUID NOT NULL,
    tenant_id    UUID NOT NULL,
    row_no       INTEGER NOT NULL,
    -- The sender own key for the record, unique within the file; known once the row is validated.
    row_ref      TEXT,
    raw          JSONB NOT NULL,
    status       VARCHAR(10) NOT NULL DEFAULT 'PENDING',
    -- Every reason a row is invalid, or why it failed: [{"code": "...", "field": "...", "message": "..."}].
    errors       JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- What creating the party answered (CREATED, MATCHED_EXISTING or REVIEW_REQUIRED) and the party and case it names.
    outcome      VARCHAR(20),
    party_id     UUID,
    case_id      UUID,
    updated_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (import_id, row_no),
    CONSTRAINT mir_import_in_tenant FOREIGN KEY (tenant_id, import_id) REFERENCES master_imports (tenant_id, import_id),
    CONSTRAINT mir_ref_unique UNIQUE (import_id, row_ref),
    CONSTRAINT mir_row_positive CHECK (row_no > 0),
    CONSTRAINT mir_status_known CHECK (status IN ('PENDING', 'VALID', 'INVALID', 'IMPORTED', 'CONFLICT', 'FAILED')),
    CONSTRAINT mir_errors_array CHECK (jsonb_typeof(errors) = 'array'),
    CONSTRAINT mir_invalid_explained CHECK (status NOT IN ('INVALID', 'FAILED') OR jsonb_array_length(errors) > 0),
    CONSTRAINT mir_imported_has_party CHECK (status <> 'IMPORTED' OR party_id IS NOT NULL)
);
CREATE INDEX idx_mir_status ON master_import_rows (import_id, status, row_no);

-- The rows as received never change. A row's status moves forward only: PENDING to VALID or INVALID, VALID to its outcome.
CREATE OR REPLACE FUNCTION master_import_row_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'import rows are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.import_id <> OLD.import_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.row_no <> OLD.row_no OR (OLD.row_ref IS NOT NULL AND NEW.row_ref IS DISTINCT FROM OLD.row_ref) OR NEW.raw <> OLD.raw THEN
        RAISE EXCEPTION 'a row as received never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('VALID', 'INVALID'))
        OR (OLD.status = 'VALID' AND NEW.status IN ('IMPORTED', 'CONFLICT', 'FAILED'))) THEN
        RAISE EXCEPTION 'an import row cannot move from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('IMPORTED', 'CONFLICT', 'FAILED', 'INVALID') AND (NEW.party_id IS DISTINCT FROM OLD.party_id OR NEW.outcome IS DISTINCT FROM OLD.outcome) THEN
        RAISE EXCEPTION 'a recorded outcome does not change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER master_import_row_guard BEFORE UPDATE OR DELETE ON master_import_rows
    FOR EACH ROW EXECUTE FUNCTION master_import_row_guard();

-- An import moves forward only, and what was received and approved does not change.
CREATE OR REPLACE FUNCTION master_import_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'imports are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.kind <> OLD.kind OR NEW.content_sha256 <> OLD.content_sha256 OR NEW.row_count <> OLD.row_count
       OR NEW.uploaded_by <> OLD.uploaded_by OR NEW.source_class <> OLD.source_class OR NEW.evidence_ref <> OLD.evidence_ref OR NEW.valid_from <> OLD.valid_from THEN
        RAISE EXCEPTION 'an import as uploaded never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.approved_by IS NOT NULL AND NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
        RAISE EXCEPTION 'an approval does not change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
        (OLD.status = 'UPLOADED' AND NEW.status IN ('VALIDATING', 'CANCELLED', 'FAILED'))
        OR (OLD.status = 'VALIDATING' AND NEW.status IN ('PREVIEW_READY', 'BLOCKED', 'CANCELLED', 'FAILED'))
        OR (OLD.status = 'PREVIEW_READY' AND NEW.status IN ('APPROVED', 'CANCELLED'))
        OR (OLD.status = 'APPROVED' AND NEW.status IN ('IMPORTING', 'CANCELLED', 'FAILED'))
        OR (OLD.status = 'IMPORTING' AND NEW.status IN ('COMPLETED', 'FAILED'))) THEN
        RAISE EXCEPTION 'an import cannot move from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER master_import_guard BEFORE UPDATE OR DELETE ON master_imports
    FOR EACH ROW EXECUTE FUNCTION master_import_guard();

ALTER TABLE master_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE master_imports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON master_imports FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE master_import_rows ENABLE ROW LEVEL SECURITY;
ALTER TABLE master_import_rows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON master_import_rows FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
