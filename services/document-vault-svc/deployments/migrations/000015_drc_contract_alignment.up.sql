-- ZS-SVC-S-001 contract alignment: the columns and the one table the specification
-- names that this service did not yet persist. Every column is nullable or has a
-- default, so existing rows and the existing routes are unaffected.
--
-- 1. document_versions.detected_content_type  (§3.3 detected_mime_type)
--    The server-side content sniff is already computed at ingest (DRC-I19) and
--    compared with the declared type; until now only the declared type was kept.
--    Rows written before this migration have no value, and none is invented.
--
-- 2. export_packages.recipient_scope, export_packages.purpose  (§9.2 POST /v1/exports:
--    "Recipient/purpose + exact scope + redaction policy"; §6.5 "Audit ... with purpose
--    and recipient scope"; §9.3 export_package_created.recipient_scope)
--    Free-text labels supplied by the caller of POST /v1/exports. Packages created
--    through the older /v1/export-packages route have neither.
--
-- 3. record_retention_states.disposition_approval_ref  (§9.2 POST
--    /v1/dispositions/{id}:approve "Consume approval decision")
--    The reference of the external approval decision that was consumed, kept as
--    evidence (DRC-I13). The approval itself — maker-checker — is owned by the
--    workflow service; this service stores the reference and does not verify it.
--
-- 4. upload_reservations  (§3.4 "Client reserves an upload with tenant, intended
--    purpose, expected size, declared content type and idempotency key"; §3.3
--    commit_status)
--    The reservation state machine. States are the subset of §3.3's commit_status
--    this service can produce: RESERVED, COMMITTED, FAILED, QUARANTINED. UPLOADING
--    and VALIDATING are not separate states because completion validates
--    synchronously in one request. A reservation is terminal once it leaves RESERVED
--    and is never edited or deleted afterwards.

-- 5. legal_hold_resolutions  (§5.6 "Hold target resolution produces an immutable
--    resolution snapshot"; §9.2 POST /v1/legal-holds/{id}:resolve-targets "Records
--    result snapshot and partial failures")
--    One append-only row per resolve call: which records became targets, which were
--    already targets, and which could not be resolved. Only the explicit-object scope
--    (a list of record ids) is resolved; the other §5.5 scope selectors need the
--    scope query language, open decision OD-S04, and are not modelled.

ALTER TABLE document_versions ADD COLUMN IF NOT EXISTS detected_content_type VARCHAR(255);

ALTER TABLE export_packages ADD COLUMN IF NOT EXISTS recipient_scope VARCHAR(255);
ALTER TABLE export_packages ADD COLUMN IF NOT EXISTS purpose VARCHAR(255);

ALTER TABLE record_retention_states ADD COLUMN IF NOT EXISTS disposition_approval_ref VARCHAR(255);

CREATE TABLE upload_reservations (
    reservation_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    document_id              UUID NOT NULL REFERENCES documents(document_id),
    reserved_by_principal_id VARCHAR(255) NOT NULL CHECK (reserved_by_principal_id <> ''),
    purpose                  VARCHAR(255) NOT NULL CHECK (purpose <> ''),
    expected_size_bytes      BIGINT       NOT NULL CHECK (expected_size_bytes > 0),
    declared_content_type    VARCHAR(255) NOT NULL CHECK (declared_content_type <> ''),
    idempotency_key          VARCHAR(255) NOT NULL CHECK (idempotency_key <> ''),
    request_hash             VARCHAR(64)  NOT NULL,
    commit_status            VARCHAR(16)  NOT NULL DEFAULT 'RESERVED'
        CHECK (commit_status IN ('RESERVED', 'COMMITTED', 'FAILED', 'QUARANTINED')),
    failure_reason           TEXT         NOT NULL DEFAULT '',
    committed_version_id     UUID REFERENCES document_versions(document_version_id),
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at             TIMESTAMPTZ,
    CONSTRAINT upload_reservations_idempotency UNIQUE (tenant_id, reserved_by_principal_id, idempotency_key),
    CONSTRAINT upload_reservations_commit_has_version CHECK ((commit_status = 'COMMITTED') = (committed_version_id IS NOT NULL))
);

CREATE INDEX idx_upload_reservations_document ON upload_reservations (tenant_id, document_id);

ALTER TABLE upload_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE upload_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON upload_reservations
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- RESERVED -> COMMITTED | FAILED | QUARANTINED, once. Everything a caller supplied
-- is immutable, a terminal reservation never changes, and nothing is deleted.
CREATE OR REPLACE FUNCTION reject_upload_reservation_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'upload_reservations rows are never deleted';
    END IF;
    IF NEW.reservation_id IS DISTINCT FROM OLD.reservation_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.document_id IS DISTINCT FROM OLD.document_id
        OR NEW.reserved_by_principal_id IS DISTINCT FROM OLD.reserved_by_principal_id
        OR NEW.purpose IS DISTINCT FROM OLD.purpose
        OR NEW.expected_size_bytes IS DISTINCT FROM OLD.expected_size_bytes
        OR NEW.declared_content_type IS DISTINCT FROM OLD.declared_content_type
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.request_hash IS DISTINCT FROM OLD.request_hash
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'upload_reservation % request data is immutable', OLD.reservation_id;
    END IF;
    IF OLD.commit_status <> 'RESERVED' THEN
        RAISE EXCEPTION 'upload_reservation % is % and immutable', OLD.reservation_id, OLD.commit_status;
    END IF;
    IF NEW.commit_status = 'RESERVED' THEN
        RAISE EXCEPTION 'upload_reservation % must move to a terminal state', OLD.reservation_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_upload_reservation_mutation
    BEFORE UPDATE OR DELETE ON upload_reservations
    FOR EACH ROW EXECUTE FUNCTION reject_upload_reservation_mutation();

CREATE TABLE legal_hold_resolutions (
    resolution_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL,
    hold_id                 UUID NOT NULL REFERENCES legal_holds(hold_id),
    resolved_by_principal_id VARCHAR(255) NOT NULL CHECK (resolved_by_principal_id <> ''),
    snapshot                JSONB NOT NULL,
    unresolved_count        INT NOT NULL CHECK (unresolved_count >= 0),
    resolved_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_legal_hold_resolutions_hold ON legal_hold_resolutions (tenant_id, hold_id, resolved_at DESC);

ALTER TABLE legal_hold_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE legal_hold_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON legal_hold_resolutions
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_legal_hold_resolution_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'legal_hold_resolutions rows are immutable snapshots and are never edited or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_legal_hold_resolution_mutation
    BEFORE UPDATE OR DELETE ON legal_hold_resolutions
    FOR EACH ROW EXECUTE FUNCTION reject_legal_hold_resolution_mutation();
