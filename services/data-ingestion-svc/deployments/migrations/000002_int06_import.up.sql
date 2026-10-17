-- 000002 ZS-SVC-M-001 INT-06: controlled bulk import.
--
-- An ImportBatch is a file that goes through scan, parse, schema validation, preview,
-- approval, execution as owning-domain commands, and reconciliation. It sits beside the
-- DATA-01 ingestion runs (000001), not inside them: different lifecycle, different
-- controls.
--
-- What the database guarantees on its own:
--   * a batch only moves forward through the lifecycle, and a terminal batch never changes;
--   * what defines a batch (profile version, entity, the declared and the computed content
--     hash) cannot be rewritten once set;
--   * an approved batch's approval is for exactly the previewed content (the fingerprints
--     are equal) and, for a material profile, was given by someone other than the creator;
--   * stored content cannot be added to after upload is sealed, and is never changed;
--   * an item cannot be marked imported without a recorded successful answer from the
--     owning domain, so no code path can claim an import it did not make;
--   * the same file cannot be imported twice for the same profile and entity;
--   * every table is tenant-isolated by row-level security, FORCEd.

-- ── import profiles (platform reference data) ───────────────────────────────
CREATE TABLE IF NOT EXISTS import_profiles (
    profile_key   VARCHAR(80)  NOT NULL CHECK (profile_key ~ '^[a-z0-9][a-z0-9._-]{2,79}$'),
    version       INTEGER      NOT NULL CHECK (version >= 1),
    import_type   VARCHAR(80)  NOT NULL CHECK (import_type <> ''),
    status        VARCHAR(20)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'CERTIFIED', 'DEPRECATED', 'WITHDRAWN')),
    material      BOOLEAN      NOT NULL,
    spec          JSONB        NOT NULL,
    spec_hash     VARCHAR(64)  NOT NULL CHECK (spec_hash ~ '^[0-9a-f]{64}$'),
    created_by    VARCHAR(255) NOT NULL,
    certified_by  VARCHAR(255),
    certified_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (profile_key, version),
    -- a certified profile was certified by someone other than its author
    CONSTRAINT import_profile_certified_by_other CHECK (
        status IN ('DRAFT', 'WITHDRAWN') OR (certified_by IS NOT NULL AND certified_at IS NOT NULL AND certified_by <> created_by))
);

CREATE OR REPLACE FUNCTION import_profile_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'import profiles are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.profile_key IS DISTINCT FROM OLD.profile_key OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'import profile identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- the specification can change only while it is a draft: a certified profile is
    -- what every batch pins, and changing it would change the meaning of past imports
    IF OLD.status <> 'DRAFT' AND (NEW.spec IS DISTINCT FROM OLD.spec OR NEW.spec_hash IS DISTINCT FROM OLD.spec_hash
        OR NEW.material IS DISTINCT FROM OLD.material OR NEW.import_type IS DISTINCT FROM OLD.import_type) THEN
        RAISE EXCEPTION 'a certified import profile is immutable; publish a new version' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
        (OLD.status = 'DRAFT' AND NEW.status IN ('CERTIFIED', 'WITHDRAWN'))
        OR (OLD.status = 'CERTIFIED' AND NEW.status IN ('DEPRECATED', 'WITHDRAWN'))
        OR (OLD.status = 'DEPRECATED' AND NEW.status = 'WITHDRAWN')) THEN
        RAISE EXCEPTION 'import profile status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_import_profile_guard ON import_profiles;
CREATE TRIGGER trg_import_profile_guard BEFORE UPDATE OR DELETE ON import_profiles
    FOR EACH ROW EXECUTE FUNCTION import_profile_guard();

-- ── batches ─────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS import_batches (
    batch_id               VARCHAR(64)  PRIMARY KEY CHECK (batch_id ~ '^imb_[0-9a-f-]{36}$'),
    tenant_id              VARCHAR(64)  NOT NULL,
    legal_entity_id        VARCHAR(255) NOT NULL CHECK (legal_entity_id <> ''),
    profile_key            VARCHAR(80)  NOT NULL,
    profile_version        INTEGER      NOT NULL,
    material               BOOLEAN      NOT NULL,
    source_system          VARCHAR(120) NOT NULL CHECK (source_system <> ''),
    -- the jurisdiction the request was made under (X-Jurisdiction-Context); carried on every command
    -- sent to the owning domains, which route regulatory and tax policy by it
    jurisdiction_context   VARCHAR(64)  NOT NULL DEFAULT '',
    -- the file name is evidence of what the uploader called it; it is never authoritative
    file_name              VARCHAR(255) NOT NULL DEFAULT '',
    declared_content_type  VARCHAR(120) NOT NULL,
    declared_sha256        VARCHAR(64)  NOT NULL CHECK (declared_sha256 ~ '^[0-9a-f]{64}$'),
    declared_rows          INTEGER      CHECK (declared_rows IS NULL OR declared_rows >= 0),
    declared_control_total VARCHAR(64),
    status                 VARCHAR(30)  NOT NULL DEFAULT 'CREATED' CHECK (status IN
        ('CREATED', 'UPLOADING', 'UPLOADED', 'SCANNED', 'PARSED', 'VALIDATED', 'PREVIEWED', 'APPROVED', 'IMPORTING',
         'COMPLETED', 'COMPLETED_WITH_EXCEPTIONS', 'RECONCILED', 'QUARANTINED', 'FAILED', 'CANCELLED')),
    status_reason          VARCHAR(80)  NOT NULL DEFAULT '',
    version                INTEGER      NOT NULL DEFAULT 1 CHECK (version >= 1),

    chunk_count            INTEGER      NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
    size_bytes             BIGINT       NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    content_sha256         VARCHAR(64),
    chain_hash             VARCHAR(64),

    validate_requested_at  TIMESTAMPTZ,
    scan_engine            VARCHAR(120),
    scan_verdict           VARCHAR(20)  CHECK (scan_verdict IS NULL OR scan_verdict IN ('CLEAN', 'INFECTED')),
    scan_signature         VARCHAR(200),
    scanned_at             TIMESTAMPTZ,

    row_count              INTEGER      NOT NULL DEFAULT 0 CHECK (row_count >= 0),
    valid_rows             INTEGER      NOT NULL DEFAULT 0 CHECK (valid_rows >= 0),
    invalid_rows           INTEGER      NOT NULL DEFAULT 0 CHECK (invalid_rows >= 0),
    computed_control_total VARCHAR(64),
    mapping_hash           VARCHAR(64),

    preview                JSONB,
    preview_fingerprint    VARCHAR(64),
    previewed_at           TIMESTAMPTZ,
    approved_fingerprint   VARCHAR(64),
    approved_by            VARCHAR(255),
    approved_at            TIMESTAMPTZ,
    executed_by            VARCHAR(255),
    executed_at            TIMESTAMPTZ,
    completed_at           TIMESTAMPTZ,

    imported_items         INTEGER      NOT NULL DEFAULT 0 CHECK (imported_items >= 0),
    quarantined_items      INTEGER      NOT NULL DEFAULT 0 CHECK (quarantined_items >= 0),
    failed_items           INTEGER      NOT NULL DEFAULT 0 CHECK (failed_items >= 0),
    skipped_items          INTEGER      NOT NULL DEFAULT 0 CHECK (skipped_items >= 0),
    reconciliation         JSONB,
    reconciled_at          TIMESTAMPTZ,

    created_by             VARCHAR(255) NOT NULL,
    correlation_id         VARCHAR(255) NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),

    FOREIGN KEY (profile_key, profile_version) REFERENCES import_profiles (profile_key, version),
    -- a material import is approved by someone other than the person who created it
    CONSTRAINT import_batch_material_sod CHECK (NOT material OR approved_by IS NULL OR approved_by <> created_by),
    -- the approval is for exactly the content that was previewed
    CONSTRAINT import_batch_approval_matches_preview CHECK (
        status NOT IN ('APPROVED', 'IMPORTING', 'COMPLETED', 'COMPLETED_WITH_EXCEPTIONS', 'RECONCILED')
        OR (approved_fingerprint IS NOT NULL AND approved_fingerprint = preview_fingerprint AND approved_by IS NOT NULL)),
    CONSTRAINT import_batch_sealed_has_content CHECK (
        status IN ('CREATED', 'UPLOADING', 'CANCELLED', 'QUARANTINED', 'FAILED') OR (content_sha256 IS NOT NULL AND chain_hash IS NOT NULL)),
    CONSTRAINT import_batch_counts_add_up CHECK (valid_rows + invalid_rows <= row_count)
);
-- The same file for the same profile and entity cannot be imported twice while a prior
-- attempt is still alive (INT-06: batch id and source hash prevent accidental duplicates).
CREATE UNIQUE INDEX IF NOT EXISTS import_batches_no_duplicate
    ON import_batches (tenant_id, legal_entity_id, profile_key, declared_sha256)
    WHERE status NOT IN ('CANCELLED', 'FAILED', 'QUARANTINED');
CREATE INDEX IF NOT EXISTS idx_import_batches_tenant_status ON import_batches (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_import_batches_validate ON import_batches (validate_requested_at)
    WHERE validate_requested_at IS NOT NULL AND status IN ('UPLOADED', 'SCANNED', 'PARSED');

CREATE OR REPLACE FUNCTION import_batch_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'import batches are evidence and are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('RECONCILED', 'QUARANTINED', 'FAILED', 'CANCELLED') THEN
        RAISE EXCEPTION 'import batch % is % and cannot change', OLD.batch_id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR NEW.profile_key IS DISTINCT FROM OLD.profile_key
       OR NEW.profile_version IS DISTINCT FROM OLD.profile_version OR NEW.material IS DISTINCT FROM OLD.material
       OR NEW.jurisdiction_context IS DISTINCT FROM OLD.jurisdiction_context
       OR NEW.source_system IS DISTINCT FROM OLD.source_system OR NEW.declared_sha256 IS DISTINCT FROM OLD.declared_sha256
       OR NEW.declared_content_type IS DISTINCT FROM OLD.declared_content_type OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.declared_rows IS DISTINCT FROM OLD.declared_rows
       OR NEW.declared_control_total IS DISTINCT FROM OLD.declared_control_total THEN
        RAISE EXCEPTION 'what defines an import batch is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- the sealed content is set once and never rewritten
    IF OLD.content_sha256 IS NOT NULL AND (NEW.content_sha256 IS DISTINCT FROM OLD.content_sha256
        OR NEW.chain_hash IS DISTINCT FROM OLD.chain_hash OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
        OR NEW.chunk_count IS DISTINCT FROM OLD.chunk_count) THEN
        RAISE EXCEPTION 'the sealed content of an import batch is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'import batch version cannot go backwards' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- the approval is set once, and only when the batch is approved
    IF OLD.approved_fingerprint IS NOT NULL AND NEW.approved_fingerprint IS DISTINCT FROM OLD.approved_fingerprint THEN
        RAISE EXCEPTION 'an import approval is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
        (OLD.status = 'CREATED'   AND NEW.status IN ('UPLOADING', 'CANCELLED'))
        OR (OLD.status = 'UPLOADING' AND NEW.status IN ('UPLOADED', 'QUARANTINED', 'CANCELLED'))
        OR (OLD.status = 'UPLOADED'  AND NEW.status IN ('SCANNED', 'QUARANTINED', 'FAILED', 'CANCELLED'))
        OR (OLD.status = 'SCANNED'   AND NEW.status IN ('PARSED', 'FAILED', 'QUARANTINED', 'CANCELLED'))
        OR (OLD.status = 'PARSED'    AND NEW.status IN ('VALIDATED', 'FAILED', 'CANCELLED'))
        OR (OLD.status = 'VALIDATED' AND NEW.status IN ('PREVIEWED', 'FAILED', 'CANCELLED'))
        OR (OLD.status = 'PREVIEWED' AND NEW.status IN ('APPROVED', 'FAILED', 'CANCELLED'))
        OR (OLD.status = 'APPROVED'  AND NEW.status IN ('IMPORTING', 'FAILED', 'CANCELLED'))
        OR (OLD.status = 'IMPORTING' AND NEW.status IN ('COMPLETED', 'COMPLETED_WITH_EXCEPTIONS', 'FAILED'))
        OR (OLD.status = 'COMPLETED_WITH_EXCEPTIONS' AND NEW.status IN ('IMPORTING', 'COMPLETED'))
        OR (OLD.status = 'COMPLETED' AND NEW.status = 'RECONCILED')) THEN
        RAISE EXCEPTION 'import batch status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_import_batch_guard ON import_batches;
CREATE TRIGGER trg_import_batch_guard BEFORE UPDATE OR DELETE ON import_batches
    FOR EACH ROW EXECUTE FUNCTION import_batch_guard();

-- ── content, chunked and hash-chained ───────────────────────────────────────
CREATE TABLE IF NOT EXISTS import_chunks (
    batch_id     VARCHAR(64) NOT NULL REFERENCES import_batches (batch_id),
    chunk_index  INTEGER     NOT NULL CHECK (chunk_index >= 0 AND chunk_index < 16384),
    tenant_id    VARCHAR(64) NOT NULL,
    content      BYTEA       NOT NULL,
    size_bytes   INTEGER     NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 8388608),
    chunk_sha256 VARCHAR(64) NOT NULL CHECK (chunk_sha256 ~ '^[0-9a-f]{64}$'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (batch_id, chunk_index),
    CONSTRAINT import_chunk_size_matches CHECK (octet_length(content) = size_bytes)
);

-- Content is added only while the upload is open, and is never changed or removed:
-- the immutable source a deterministic rerun reads (INT-06 NFR).
CREATE OR REPLACE FUNCTION import_chunk_guard() RETURNS TRIGGER AS $$
DECLARE st VARCHAR(30);
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'import content is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT status INTO st FROM import_batches WHERE batch_id = NEW.batch_id;
    IF st IS NULL OR st NOT IN ('CREATED', 'UPLOADING') THEN
        RAISE EXCEPTION 'content cannot be added to an import batch that is %', COALESCE(st, 'missing') USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_import_chunk_guard ON import_chunks;
CREATE TRIGGER trg_import_chunk_guard BEFORE INSERT OR UPDATE OR DELETE ON import_chunks
    FOR EACH ROW EXECUTE FUNCTION import_chunk_guard();

-- ── items ───────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS import_items (
    batch_id          VARCHAR(64)  NOT NULL REFERENCES import_batches (batch_id),
    row_no            INTEGER      NOT NULL CHECK (row_no >= 1),
    tenant_id         VARCHAR(64)  NOT NULL,
    row_hash          VARCHAR(64)  NOT NULL,
    item_values       JSONB        NOT NULL,
    status            VARCHAR(20)  NOT NULL CHECK (status IN
        ('VALID', 'INVALID', 'SKIPPED', 'PENDING', 'IMPORTED', 'QUARANTINED', 'FAILED')),
    error_codes       JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- the owning domain's idempotency key for this item: deterministic, so a rerun or a
    -- retried delivery is recognised by the domain instead of creating a duplicate
    idempotency_key   VARCHAR(128) NOT NULL,
    attempts          INTEGER      NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- how many times a person put the item back in the queue after it was quarantined or failed
    retries           INTEGER      NOT NULL DEFAULT 0 CHECK (retries >= 0),
    next_attempt_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    locked_until      TIMESTAMPTZ,
    domain_status     INTEGER,
    result_ref        VARCHAR(255),
    last_error_class  VARCHAR(60),
    quarantine_reason VARCHAR(80),
    quarantine_owner  VARCHAR(255),
    quarantined_by    VARCHAR(255),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (batch_id, row_no),
    -- an item is imported only with a recorded successful answer from the owning domain
    CONSTRAINT import_item_imported_has_domain_success CHECK (
        status <> 'IMPORTED' OR (domain_status BETWEEN 200 AND 299)),
    CONSTRAINT import_item_quarantine_has_reason_and_owner CHECK (
        status <> 'QUARANTINED' OR (quarantine_reason IS NOT NULL AND quarantine_owner IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_import_items_status ON import_items (batch_id, status);
CREATE INDEX IF NOT EXISTS idx_import_items_due ON import_items (next_attempt_at) WHERE status = 'PENDING';

CREATE OR REPLACE FUNCTION import_item_guard() RETURNS TRIGGER AS $$
DECLARE st VARCHAR(30);
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- rows are rebuilt only while the batch is being parsed (a crashed parse is rerun)
        SELECT status INTO st FROM import_batches WHERE batch_id = OLD.batch_id;
        IF st IS DISTINCT FROM 'SCANNED' THEN
            RAISE EXCEPTION 'import items are evidence and are never deleted' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id OR NEW.row_no IS DISTINCT FROM OLD.row_no
       OR NEW.row_hash IS DISTINCT FROM OLD.row_hash OR NEW.item_values IS DISTINCT FROM OLD.item_values
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
        RAISE EXCEPTION 'import item identity and content are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
        (OLD.status = 'VALID'       AND NEW.status IN ('PENDING', 'SKIPPED'))
        OR (OLD.status = 'INVALID'  AND NEW.status = 'SKIPPED')
        OR (OLD.status = 'PENDING'  AND NEW.status IN ('IMPORTED', 'QUARANTINED', 'FAILED'))
        OR (OLD.status IN ('QUARANTINED', 'FAILED') AND NEW.status = 'PENDING')) THEN
        RAISE EXCEPTION 'import item status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'IMPORTED' AND (NEW.status <> 'IMPORTED' OR NEW.domain_status IS DISTINCT FROM OLD.domain_status
        OR NEW.result_ref IS DISTINCT FROM OLD.result_ref) THEN
        RAISE EXCEPTION 'an imported item never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_import_item_guard ON import_items;
CREATE TRIGGER trg_import_item_guard BEFORE UPDATE OR DELETE ON import_items
    FOR EACH ROW EXECUTE FUNCTION import_item_guard();

-- ── the batch's append-only history ─────────────────────────────────────────
CREATE TABLE IF NOT EXISTS import_batch_log (
    log_id         BIGSERIAL    PRIMARY KEY,
    tenant_id      VARCHAR(64)  NOT NULL,
    batch_id       VARCHAR(64)  NOT NULL REFERENCES import_batches (batch_id),
    kind           VARCHAR(40)  NOT NULL,
    actor          VARCHAR(255) NOT NULL,
    from_status    VARCHAR(30),
    to_status      VARCHAR(30),
    reason         VARCHAR(200) NOT NULL DEFAULT '',
    detail         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    correlation_id VARCHAR(255) NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_import_batch_log_batch ON import_batch_log (batch_id, log_id);

CREATE OR REPLACE FUNCTION import_log_immutable() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'import_batch_log is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_import_log_immutable ON import_batch_log;
CREATE TRIGGER trg_import_log_immutable BEFORE UPDATE OR DELETE ON import_batch_log
    FOR EACH ROW EXECUTE FUNCTION import_log_immutable();

-- ── row-level security ──────────────────────────────────────────────────────
-- Profiles are platform reference data: any tenant may read them, and only the platform
-- scope may write them.
ALTER TABLE import_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE import_profiles FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS import_profiles_read ON import_profiles;
CREATE POLICY import_profiles_read ON import_profiles FOR SELECT USING (true);
DROP POLICY IF EXISTS import_profiles_platform_write ON import_profiles;
CREATE POLICY import_profiles_platform_write ON import_profiles FOR INSERT
    WITH CHECK (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001');
DROP POLICY IF EXISTS import_profiles_platform_update ON import_profiles;
CREATE POLICY import_profiles_platform_update ON import_profiles FOR UPDATE
    USING (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001')
    WITH CHECK (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001');

ALTER TABLE import_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE import_batches FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS import_batches_tenant ON import_batches;
CREATE POLICY import_batches_tenant ON import_batches FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
-- The workers discover due work across tenants from one loop. They name themselves
-- (app.import_worker) instead of running unscoped: the flag admits them to SELECT and
-- UPDATE batches and items to claim work, and to nothing else. Every batch is then
-- processed in a tenant-scoped transaction.
DROP POLICY IF EXISTS import_batches_worker_read ON import_batches;
CREATE POLICY import_batches_worker_read ON import_batches FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.import_worker', true), ''), 'false') = 'true');

ALTER TABLE import_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE import_chunks FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS import_chunks_tenant ON import_chunks;
CREATE POLICY import_chunks_tenant ON import_chunks FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE import_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE import_items FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS import_items_tenant ON import_items;
CREATE POLICY import_items_tenant ON import_items FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
DROP POLICY IF EXISTS import_items_worker_read ON import_items;
CREATE POLICY import_items_worker_read ON import_items FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.import_worker', true), ''), 'false') = 'true');

ALTER TABLE import_batch_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE import_batch_log FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS import_batch_log_tenant ON import_batch_log;
CREATE POLICY import_batch_log_tenant ON import_batch_log FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ── the outbox relay (a fix to 000001's outbox_events) ──────────────────────
-- The relay polls for unpublished events across every tenant. outbox_events is FORCE ROW LEVEL
-- SECURITY with a tenant-only policy, so a relay that runs unscoped sees NOTHING when the service
-- connects as an unprivileged role: every event, DATA-01's and INT-06's, stayed unpublished with no
-- error to say so (found in a live run). The relay now names itself (app.outbox_relay) inside the
-- transaction it works in, and this admits it to read and mark rows and to nothing else.
DROP POLICY IF EXISTS outbox_events_relay_read ON outbox_events;
CREATE POLICY outbox_events_relay_read ON outbox_events FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true');
DROP POLICY IF EXISTS outbox_events_relay_mark ON outbox_events;
CREATE POLICY outbox_events_relay_mark ON outbox_events FOR UPDATE
    USING (COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true');
