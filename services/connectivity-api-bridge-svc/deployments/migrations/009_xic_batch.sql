-- 009 ZS-SVC-T-001 XIC-05: Batch/File Exchange and Controlled Replay.
--
-- A batch is the durable record of one file delivery; its files and rows keep their own truth (file-level and row-level
-- outcomes are never one status, §10.2). A replay job is the governed record of re-running held exchanges.
--
-- What the database guarantees on its own:
--   * tenant isolation (RLS, FORCEd) on every table;
--   * a batch identity (tenant, dedupe key) is held once: a repeat is the same batch and a conflicting arrival is
--     kept apart as its own QUARANTINED row, never merged;
--   * a batch's identity, hashes and contract pin never change; its state moves only along the documented graph, and
--     REJECTED and QUARANTINED are final;
--   * a file's identity and digests never change, and its stored body can only be removed (by processing or by the
--     purge worker after retention), never rewritten;
--   * a row is identified by (file, row number) once, keeps its hash, and keeps the exchange it became;
--   * a replay job's scope, versions, reason and risk are fixed at request; a HIGH-risk job cannot be approved,
--     run or completed without an approver who is not its requester, and cannot be requested without a dry run;
--   * evidence is append-only.

CREATE TABLE IF NOT EXISTS xic_batches (
    batch_id             UUID         PRIMARY KEY,
    tenant_id            VARCHAR(64)  NOT NULL,
    connection_id        UUID         NOT NULL REFERENCES xic_connections(connection_id),
    connector_id         VARCHAR(63)  NOT NULL,
    operation_id         VARCHAR(63)  NOT NULL,
    contract_version_id  UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    mapping_version_id   UUID         REFERENCES xic_mapping_versions(mapping_version_id),
    external_batch_id    VARCHAR(128) NOT NULL,
    source_system        VARCHAR(64)  NOT NULL DEFAULT '',
    source_schema        VARCHAR(80)  NOT NULL DEFAULT '',
    channel              VARCHAR(5)   NOT NULL,
    state                VARCHAR(11)  NOT NULL,
    reason_code          VARCHAR(64)  NOT NULL DEFAULT '',
    xic_code             VARCHAR(8)   NOT NULL DEFAULT '',
    manifest_hash        VARCHAR(64)  NOT NULL,
    content_hash         VARCHAR(64)  NOT NULL,
    dedupe_key           VARCHAR(64),
    original_batch_id    UUID         REFERENCES xic_batches(batch_id),
    manifest             JSONB,
    file_count           INTEGER      NOT NULL DEFAULT 0,
    declared_rows        INTEGER,
    parsed_rows          INTEGER      NOT NULL DEFAULT 0,
    admitted_rows        INTEGER      NOT NULL DEFAULT 0,
    accepted_rows        INTEGER      NOT NULL DEFAULT 0,
    pending_rows         INTEGER      NOT NULL DEFAULT 0,
    rejected_rows        INTEGER      NOT NULL DEFAULT 0,
    duplicate_rows       INTEGER      NOT NULL DEFAULT 0,
    quarantined_rows     INTEGER      NOT NULL DEFAULT 0,
    domain_rejected_rows INTEGER      NOT NULL DEFAULT 0,
    variance_rows        INTEGER      NOT NULL DEFAULT 0,
    processing_done      BOOLEAN      NOT NULL DEFAULT false,
    auth_mechanism       VARCHAR(24)  NOT NULL DEFAULT '',
    source_net_hash      VARCHAR(64)  NOT NULL DEFAULT '',
    correlation_id       VARCHAR(128) NOT NULL DEFAULT '',
    lease_until          TIMESTAMPTZ,
    next_process_at      TIMESTAMPTZ,
    process_attempts     INTEGER      NOT NULL DEFAULT 0,
    version              BIGINT       NOT NULL DEFAULT 1,
    received_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_b_dedupe UNIQUE (tenant_id, dedupe_key),
    CONSTRAINT xic_b_channel_valid CHECK (channel IN ('HTTPS', 'SFTP')),
    CONSTRAINT xic_b_state_valid CHECK (state IN ('RECEIVED', 'VALIDATING', 'ACCEPTED', 'PARTIAL', 'REJECTED', 'PROCESSING', 'RECONCILED', 'QUARANTINED')),
    CONSTRAINT xic_b_hashes CHECK (manifest_hash ~ '^[0-9a-f]{64}$' AND content_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_b_stopped_reasoned CHECK (state NOT IN ('REJECTED', 'QUARANTINED') OR reason_code <> ''),
    CONSTRAINT xic_b_conflict_points_back CHECK (reason_code <> 'BATCH_ID_CONFLICT' OR (state = 'QUARANTINED' AND original_batch_id IS NOT NULL AND dedupe_key IS NULL)),
    CONSTRAINT xic_b_counts CHECK (file_count >= 0 AND parsed_rows >= 0 AND admitted_rows >= 0 AND accepted_rows >= 0 AND pending_rows >= 0 AND rejected_rows >= 0
        AND duplicate_rows >= 0 AND quarantined_rows >= 0 AND domain_rejected_rows >= 0 AND process_attempts >= 0 AND version >= 1),
    CONSTRAINT xic_b_reporting_after_admission CHECK (state NOT IN ('PARTIAL', 'RECONCILED') OR processing_done),
    CONSTRAINT xic_b_reconciled_clean CHECK (state <> 'RECONCILED' OR (pending_rows = 0 AND rejected_rows = 0 AND quarantined_rows = 0 AND domain_rejected_rows = 0 AND variance_rows = 0))
);
CREATE INDEX IF NOT EXISTS xic_b_conn ON xic_batches (connection_id, received_at);
CREATE INDEX IF NOT EXISTS xic_b_due ON xic_batches (next_process_at) WHERE state IN ('RECEIVED', 'PROCESSING', 'PARTIAL');

CREATE OR REPLACE FUNCTION xic_batch_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a batch is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.batch_id <> OLD.batch_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.connection_id <> OLD.connection_id OR NEW.connector_id <> OLD.connector_id
       OR NEW.operation_id <> OLD.operation_id OR NEW.contract_version_id <> OLD.contract_version_id OR NEW.external_batch_id <> OLD.external_batch_id
       OR NEW.manifest_hash <> OLD.manifest_hash OR NEW.content_hash <> OLD.content_hash OR NEW.dedupe_key IS DISTINCT FROM OLD.dedupe_key
       OR NEW.original_batch_id IS DISTINCT FROM OLD.original_batch_id OR NEW.channel <> OLD.channel OR NEW.received_at <> OLD.received_at THEN
        RAISE EXCEPTION 'the identity and hashes of a batch are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'batch version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('REJECTED', 'QUARANTINED') THEN
        RAISE EXCEPTION 'a % batch is final', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> NEW.state AND NOT (
           (OLD.state = 'RECEIVED'   AND NEW.state IN ('VALIDATING', 'REJECTED', 'QUARANTINED'))
        OR (OLD.state = 'VALIDATING' AND NEW.state IN ('ACCEPTED', 'REJECTED', 'QUARANTINED'))
        OR (OLD.state = 'ACCEPTED'   AND NEW.state IN ('PROCESSING', 'REJECTED', 'QUARANTINED'))
        OR (OLD.state = 'PROCESSING' AND NEW.state IN ('PARTIAL', 'RECONCILED', 'QUARANTINED'))
        OR (OLD.state = 'PARTIAL'    AND NEW.state IN ('PROCESSING', 'RECONCILED'))
        OR (OLD.state = 'RECONCILED' AND NEW.state IN ('PROCESSING', 'PARTIAL'))) THEN
        RAISE EXCEPTION 'batch state cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.processing_done AND NOT NEW.processing_done THEN
        RAISE EXCEPTION 'a batch whose rows were admitted does not go back' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_batch_guard_trg ON xic_batches;
CREATE TRIGGER xic_batch_guard_trg BEFORE UPDATE OR DELETE ON xic_batches FOR EACH ROW EXECUTE FUNCTION xic_batch_guard();

CREATE TABLE IF NOT EXISTS xic_batch_files (
    file_id            UUID         PRIMARY KEY,
    batch_id           UUID         NOT NULL REFERENCES xic_batches(batch_id),
    tenant_id          VARCHAR(64)  NOT NULL,
    name               VARCHAR(255) NOT NULL,
    ordinal            INTEGER      NOT NULL DEFAULT 0,
    declared_sha256    VARCHAR(64)  NOT NULL,
    declared_size      BIGINT       NOT NULL,
    compression        VARCHAR(5)   NOT NULL DEFAULT 'none',
    actual_sha256      VARCHAR(64)  NOT NULL DEFAULT '',
    actual_size        BIGINT       NOT NULL DEFAULT 0,
    declared_records   INTEGER,
    parsed_rows        INTEGER      NOT NULL DEFAULT 0,
    state              VARCHAR(8)   NOT NULL DEFAULT 'RECEIVED',
    reason_code        VARCHAR(64)  NOT NULL DEFAULT '',
    content            BYTEA,
    content_expires_at TIMESTAMPTZ,
    CONSTRAINT xic_bf_name UNIQUE (batch_id, name),
    CONSTRAINT xic_bf_state_valid CHECK (state IN ('RECEIVED', 'VERIFIED', 'REJECTED')),
    CONSTRAINT xic_bf_declared CHECK (declared_sha256 ~ '^[0-9a-f]{64}$' AND declared_size >= 0 AND actual_size >= 0 AND parsed_rows >= 0),
    CONSTRAINT xic_bf_actual CHECK (actual_sha256 = '' OR actual_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_bf_content_dated CHECK (content IS NULL OR content_expires_at IS NOT NULL),
    CONSTRAINT xic_bf_verified_matches CHECK (state <> 'VERIFIED' OR (actual_sha256 = declared_sha256 AND actual_size = declared_size))
);
CREATE INDEX IF NOT EXISTS xic_bf_batch ON xic_batch_files (batch_id, ordinal);
CREATE INDEX IF NOT EXISTS xic_bf_purge ON xic_batch_files (content_expires_at) WHERE content IS NOT NULL;

CREATE OR REPLACE FUNCTION xic_bf_guard() RETURNS trigger AS $$
DECLARE
    purge BOOLEAN := COALESCE(current_setting('app.xic_purge', true), '') = 'true';
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a batch file record is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.file_id <> OLD.file_id OR NEW.batch_id <> OLD.batch_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.name <> OLD.name
       OR NEW.declared_sha256 <> OLD.declared_sha256 OR NEW.declared_size <> OLD.declared_size OR NEW.actual_sha256 <> OLD.actual_sha256
       OR NEW.actual_size <> OLD.actual_size OR NEW.compression <> OLD.compression OR NEW.ordinal <> OLD.ordinal THEN
        RAISE EXCEPTION 'the identity and digests of a file are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.content IS NOT NULL AND NEW.content IS DISTINCT FROM OLD.content THEN
        RAISE EXCEPTION 'a stored body is kept or removed, never rewritten' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF purge THEN
        IF OLD.content IS NULL OR NEW.content IS NOT NULL OR OLD.content_expires_at > now() OR NEW.state <> OLD.state THEN
            RAISE EXCEPTION 'the purge worker removes expired bodies only' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_bf_guard_trg ON xic_batch_files;
CREATE TRIGGER xic_bf_guard_trg BEFORE UPDATE OR DELETE ON xic_batch_files FOR EACH ROW EXECUTE FUNCTION xic_bf_guard();

CREATE TABLE IF NOT EXISTS xic_batch_rows (
    row_id          UUID         PRIMARY KEY,
    batch_id        UUID         NOT NULL REFERENCES xic_batches(batch_id),
    file_id         UUID         NOT NULL REFERENCES xic_batch_files(file_id),
    tenant_id       VARCHAR(64)  NOT NULL,
    row_no          INTEGER      NOT NULL,
    source_row_hash VARCHAR(64)  NOT NULL,
    exchange_id     UUID         REFERENCES xic_inbound_exchanges(exchange_id),
    state           VARCHAR(15)  NOT NULL,
    reason_code     VARCHAR(64)  NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_br_position UNIQUE (file_id, row_no),
    CONSTRAINT xic_br_state_valid CHECK (state IN ('PENDING', 'ACCEPTED', 'DOMAIN_REJECTED', 'QUARANTINED', 'REJECTED', 'DUPLICATE')),
    CONSTRAINT xic_br_hash CHECK (source_row_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_br_rownum CHECK (row_no >= 1),
    CONSTRAINT xic_br_admitted_has_exchange CHECK (state IN ('REJECTED') OR exchange_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS xic_br_batch ON xic_batch_rows (batch_id, state, row_no);
CREATE INDEX IF NOT EXISTS xic_br_exchange ON xic_batch_rows (exchange_id) WHERE exchange_id IS NOT NULL;

CREATE OR REPLACE FUNCTION xic_br_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a batch row is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.row_id <> OLD.row_id OR NEW.batch_id <> OLD.batch_id OR NEW.file_id <> OLD.file_id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.row_no <> OLD.row_no OR NEW.source_row_hash <> OLD.source_row_hash OR NEW.exchange_id IS DISTINCT FROM OLD.exchange_id THEN
        RAISE EXCEPTION 'the identity, hash and exchange of a row are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_br_guard_trg ON xic_batch_rows;
CREATE TRIGGER xic_br_guard_trg BEFORE UPDATE OR DELETE ON xic_batch_rows FOR EACH ROW EXECUTE FUNCTION xic_br_guard();

CREATE TABLE IF NOT EXISTS xic_batch_events (
    ord        BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id   UUID         NOT NULL REFERENCES xic_batches(batch_id),
    tenant_id  VARCHAR(64)  NOT NULL,
    kind       VARCHAR(40)  NOT NULL,
    from_state VARCHAR(11)  NOT NULL DEFAULT '',
    to_state   VARCHAR(11)  NOT NULL DEFAULT '',
    detail     JSONB,
    actor      VARCHAR(255) NOT NULL,
    at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS xic_be_batch ON xic_batch_events (batch_id, ord);
DROP TRIGGER IF EXISTS xic_batch_events_no_change ON xic_batch_events;
CREATE TRIGGER xic_batch_events_no_change BEFORE UPDATE OR DELETE ON xic_batch_events FOR EACH ROW EXECUTE FUNCTION xic_append_only();

-- ── replay jobs ──────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS xic_replay_jobs (
    replay_id           UUID         PRIMARY KEY,
    tenant_id           VARCHAR(64)  NOT NULL,
    connection_id       UUID         REFERENCES xic_connections(connection_id),
    operation_id        VARCHAR(63)  NOT NULL DEFAULT '',
    batch_id            UUID         REFERENCES xic_batches(batch_id),
    scope_kind          VARCHAR(9)   NOT NULL,
    scope               JSONB        NOT NULL,
    reason_code         VARCHAR(24)  NOT NULL,
    reason              VARCHAR(255) NOT NULL,
    dry_run             BOOLEAN      NOT NULL DEFAULT false,
    dry_run_of          UUID         REFERENCES xic_replay_jobs(replay_id),
    use_current_mapping BOOLEAN      NOT NULL DEFAULT false,
    state               VARCHAR(16)  NOT NULL,
    risk                VARCHAR(4)   NOT NULL,
    risk_reasons        JSONB,
    contract_version_id UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    mapping_version_id  UUID         REFERENCES xic_mapping_versions(mapping_version_id),
    selected_items      INTEGER      NOT NULL,
    projected           JSONB,
    result              JSONB,
    requested_by        VARCHAR(255) NOT NULL,
    decided_by          VARCHAR(255),
    decided_at          TIMESTAMPTZ,
    decision_note       VARCHAR(255) NOT NULL DEFAULT '',
    correlation_id      VARCHAR(128) NOT NULL DEFAULT '',
    lease_until         TIMESTAMPTZ,
    version             BIGINT       NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    CONSTRAINT xic_rj_scope_valid CHECK (scope_kind IN ('BATCH', 'EXCHANGES', 'WINDOW')),
    CONSTRAINT xic_rj_reason_valid CHECK (reason_code IN ('INCIDENT_REPAIR', 'SOURCE_CORRECTION', 'PROVIDER_REDELIVERY', 'MIGRATION_VALIDATION', 'APPROVED_BACKFILL')),
    CONSTRAINT xic_rj_reasoned CHECK (btrim(reason) <> ''),
    CONSTRAINT xic_rj_state_valid CHECK (state IN ('DRY_RUN_COMPLETE', 'PENDING_APPROVAL', 'APPROVED', 'RUNNING', 'COMPLETED', 'REJECTED', 'CANCELLED', 'FAILED')),
    CONSTRAINT xic_rj_risk_valid CHECK (risk IN ('LOW', 'HIGH')),
    CONSTRAINT xic_rj_selected CHECK (selected_items >= 1 AND version >= 1),
    CONSTRAINT xic_rj_dry_run_state CHECK (dry_run = (state = 'DRY_RUN_COMPLETE')),
    -- a replay is requested after a dry run of the same scope when its risk is high
    CONSTRAINT xic_rj_high_needs_dry_run CHECK (dry_run OR risk = 'LOW' OR dry_run_of IS NOT NULL),
    -- maker-checker: whoever decides a replay is not whoever asked for it, and a HIGH-risk job does not get past
    -- approval without an approver
    CONSTRAINT xic_rj_distinct_people CHECK (decided_by IS NULL OR decided_by <> requested_by),
    CONSTRAINT xic_rj_high_needs_approver CHECK (risk = 'LOW' OR state NOT IN ('APPROVED', 'RUNNING', 'COMPLETED') OR decided_by IS NOT NULL),
    CONSTRAINT xic_rj_current_mapping_is_high CHECK (NOT use_current_mapping OR risk = 'HIGH')
);
CREATE INDEX IF NOT EXISTS xic_rj_due ON xic_replay_jobs (state) WHERE state IN ('APPROVED', 'RUNNING');
CREATE INDEX IF NOT EXISTS xic_rj_batch ON xic_replay_jobs (batch_id, created_at) WHERE batch_id IS NOT NULL;

CREATE OR REPLACE FUNCTION xic_rj_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a replay job is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.replay_id <> OLD.replay_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.scope_kind <> OLD.scope_kind OR NEW.scope::text <> OLD.scope::text
       OR NEW.reason_code <> OLD.reason_code OR NEW.reason <> OLD.reason OR NEW.dry_run <> OLD.dry_run OR NEW.use_current_mapping <> OLD.use_current_mapping
       OR NEW.contract_version_id <> OLD.contract_version_id OR NEW.mapping_version_id IS DISTINCT FROM OLD.mapping_version_id OR NEW.risk <> OLD.risk
       OR NEW.requested_by <> OLD.requested_by OR NEW.selected_items <> OLD.selected_items OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the scope, versions, reason and risk of a replay are fixed at request' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'replay version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('DRY_RUN_COMPLETE', 'COMPLETED', 'REJECTED', 'CANCELLED', 'FAILED') THEN
        RAISE EXCEPTION 'a % replay is final', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> NEW.state AND NOT (
           (OLD.state = 'PENDING_APPROVAL' AND NEW.state IN ('APPROVED', 'REJECTED', 'CANCELLED'))
        OR (OLD.state = 'APPROVED'         AND NEW.state IN ('RUNNING', 'CANCELLED'))
        OR (OLD.state = 'RUNNING'          AND NEW.state IN ('COMPLETED', 'FAILED'))) THEN
        RAISE EXCEPTION 'replay state cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_rj_guard_trg ON xic_replay_jobs;
CREATE TRIGGER xic_rj_guard_trg BEFORE UPDATE OR DELETE ON xic_replay_jobs FOR EACH ROW EXECUTE FUNCTION xic_rj_guard();

CREATE TABLE IF NOT EXISTS xic_replay_items (
    replay_id     UUID         NOT NULL REFERENCES xic_replay_jobs(replay_id),
    tenant_id     VARCHAR(64)  NOT NULL,
    exchange_id   UUID         NOT NULL REFERENCES xic_inbound_exchanges(exchange_id),
    before_state  VARCHAR(12)  NOT NULL DEFAULT '',
    before_class  VARCHAR(20)  NOT NULL DEFAULT '',
    before_reason VARCHAR(64)  NOT NULL DEFAULT '',
    after_state   VARCHAR(12)  NOT NULL DEFAULT '',
    after_class   VARCHAR(20)  NOT NULL DEFAULT '',
    after_reason  VARCHAR(64)  NOT NULL DEFAULT '',
    changed       BOOLEAN      NOT NULL DEFAULT false,
    applied       BOOLEAN      NOT NULL DEFAULT false,
    skipped       VARCHAR(40)  NOT NULL DEFAULT '',
    done          BOOLEAN      NOT NULL DEFAULT false,
    PRIMARY KEY (replay_id, exchange_id),
    CONSTRAINT xic_ri_applied_changed CHECK (NOT applied OR changed)
);

CREATE OR REPLACE FUNCTION xic_ri_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a replay item is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.replay_id <> OLD.replay_id OR NEW.exchange_id <> OLD.exchange_id OR NEW.tenant_id <> OLD.tenant_id THEN
        RAISE EXCEPTION 'the identity of a replay item is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.done AND NOT NEW.done THEN
        RAISE EXCEPTION 'a replayed item is not replayed again' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_ri_guard_trg ON xic_replay_items;
CREATE TRIGGER xic_ri_guard_trg BEFORE UPDATE OR DELETE ON xic_replay_items FOR EACH ROW EXECUTE FUNCTION xic_ri_guard();

-- ── row-level security ──────────────────────────────────────────────────────

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_batches', 'xic_batch_files', 'xic_batch_rows', 'xic_batch_events', 'xic_replay_jobs', 'xic_replay_items']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_tenant', t);
        EXECUTE format($p$CREATE POLICY %I ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))$p$, t || '_tenant', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_worker_read', t);
        EXECUTE format($p$CREATE POLICY %I ON %I FOR SELECT
            USING (COALESCE(NULLIF(current_setting('app.xic_worker', true), ''), 'false') = 'true')$p$, t || '_worker_read', t);
    END LOOP;
END $$;
-- the purge worker may remove expired file bodies across tenants; the guard trigger limits what that can change
DROP POLICY IF EXISTS xic_batch_files_purge ON xic_batch_files;
CREATE POLICY xic_batch_files_purge ON xic_batch_files FOR UPDATE
    USING (COALESCE(NULLIF(current_setting('app.xic_purge', true), ''), 'false') = 'true')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.xic_purge', true), ''), 'false') = 'true');
