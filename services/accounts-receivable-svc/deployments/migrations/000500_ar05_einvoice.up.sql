-- Migration: 000500_ar05_einvoice.up.sql
-- AR-05 Electronic Invoice (spec ZS-SVC-C-001 section 8). Render, validate and transmit an ISSUED document through a
-- jurisdiction/network profile while the source document stays authoritative (invariants 4, 5, 18).
--   ar05_einvoice_submissions   one logical submission per (document, profile) at a time; identity columns frozen by trigger
--   ar05_artifacts              the rendered artifact bytes + sha256 (append-only)
--   ar05_validation_results     every validation run (append-only)
--   ar05_submission_attempts    durable external_attempt_id per transmission try (append-only)
--   ar05_attempt_results        outcomes of an attempt, newest = current (append-only)
--   ar05_attempt_index          tenant routing for provider callbacks (no RLS, insert-only)
--   ar05_acknowledgements       provider acks/rejections with signature-verification evidence (append-only)
--   ar05_callback_nonces        replay protection for signed callbacks (no RLS, insert-only)
--   ar05_reconciliations        external-state reconciliation history (append-only)
--   ar05_submission_history     evidence of every state change (append-only)
--   ar05_security_events        refused/forged callbacks (no RLS, append-only)

CREATE TABLE IF NOT EXISTS ar05_einvoice_submissions (
    submission_id          UUID PRIMARY KEY,
    tenant_id              UUID         NOT NULL,
    legal_entity_id        UUID         NOT NULL,
    document_type          VARCHAR(16)  NOT NULL CHECK (document_type IN ('INVOICE','CREDIT_NOTE','DEBIT_NOTE')),
    document_id            UUID         NOT NULL,
    document_number        VARCHAR(64)  NOT NULL DEFAULT '',
    content_hash           VARCHAR(64)  NOT NULL CHECK (content_hash <> ''),
    jurisdiction_code      VARCHAR(32)  NOT NULL,
    profile_code           VARCHAR(128) NOT NULL,
    profile_version        VARCHAR(32)  NOT NULL,
    network                VARCHAR(64)  NOT NULL,
    syntax                 VARCHAR(32)  NOT NULL CHECK (syntax IN ('UBL_XML','JSON','AUTHORITY_SPECIFIC')),
    transmission_mode      VARCHAR(32)  NOT NULL CHECK (transmission_mode IN ('EXCHANGE','CLEARANCE','REPORTING_REALTIME','REPORTING_POST_ISSUANCE')),
    profile_snapshot       JSONB        NOT NULL,
    pack_version_id        VARCHAR(64)  NOT NULL DEFAULT '',
    pack_artifact_digest   VARCHAR(80)  NOT NULL DEFAULT '',
    rule_id                VARCHAR(64)  NOT NULL DEFAULT '',
    rule_content_digest    VARCHAR(80)  NOT NULL DEFAULT '',
    resolution_decision_id VARCHAR(64)  NOT NULL DEFAULT '',
    endpoint_id            VARCHAR(255) NOT NULL DEFAULT '',
    participant_id         VARCHAR(255) NOT NULL DEFAULT '',
    purpose                VARCHAR(64)  NOT NULL DEFAULT '',
    provider               VARCHAR(64),
    state                  VARCHAR(16)  NOT NULL DEFAULT 'PREPARED'
        CONSTRAINT ar05_state_valid CHECK (state IN ('PREPARED','VALIDATED','SUBMITTED','PENDING_UNKNOWN','ACCEPTED','REJECTED','CANCELLED')),
    validation_status      VARCHAR(16)  NOT NULL DEFAULT 'NOT_RUN' CHECK (validation_status IN ('NOT_RUN','PASSED','FAILED')),
    artifact_hash          VARCHAR(64)  NOT NULL CHECK (artifact_hash ~ '^[0-9a-f]{64}$'),
    artifact_ref           VARCHAR(255) NOT NULL,
    parent_submission_id   UUID REFERENCES ar05_einvoice_submissions(submission_id),
    registry_submission_id VARCHAR(64),
    attempt_count          INTEGER      NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_provider_ref      VARCHAR(255),
    prepared_by            VARCHAR(255) NOT NULL,
    version                INTEGER      NOT NULL DEFAULT 1,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
-- One logical submission per (document, profile) at a time (duplicate-submission suppression). A REJECTED or CANCELLED
-- one is finished and no longer blocks a chained resubmission; an ACCEPTED one keeps blocking.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar05_active_submission ON ar05_einvoice_submissions (tenant_id, document_type, document_id, profile_code)
    WHERE state NOT IN ('REJECTED','CANCELLED');
CREATE INDEX IF NOT EXISTS idx_ar05_submission_doc ON ar05_einvoice_submissions (tenant_id, document_type, document_id, created_at);

CREATE OR REPLACE FUNCTION ar05_guard_submission() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR05_SUBMISSION_NOT_DELETABLE: an e-invoice submission is never deleted';
    END IF;
    IF NEW.submission_id <> OLD.submission_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.document_type <> OLD.document_type OR NEW.document_id <> OLD.document_id OR NEW.content_hash <> OLD.content_hash
       OR NEW.jurisdiction_code <> OLD.jurisdiction_code OR NEW.profile_code <> OLD.profile_code OR NEW.profile_version <> OLD.profile_version
       OR NEW.network <> OLD.network OR NEW.syntax <> OLD.syntax OR NEW.transmission_mode <> OLD.transmission_mode
       OR NEW.profile_snapshot <> OLD.profile_snapshot OR NEW.artifact_hash <> OLD.artifact_hash OR NEW.artifact_ref <> OLD.artifact_ref
       OR NEW.parent_submission_id IS DISTINCT FROM OLD.parent_submission_id OR NEW.prepared_by <> OLD.prepared_by THEN
        RAISE EXCEPTION 'AR05_SUBMISSION_IMMUTABLE: the document binding, profile, artifact and chain of a submission never change';
    END IF;
    IF OLD.state IN ('ACCEPTED','REJECTED','CANCELLED') AND NEW.state <> OLD.state THEN
        RAISE EXCEPTION 'AR05_TERMINAL_STATE: a % submission cannot change state', OLD.state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar05_submission_guard ON ar05_einvoice_submissions;
CREATE TRIGGER ar05_submission_guard BEFORE UPDATE OR DELETE ON ar05_einvoice_submissions
    FOR EACH ROW EXECUTE FUNCTION ar05_guard_submission();

CREATE TABLE IF NOT EXISTS ar05_artifacts (
    artifact_id     UUID PRIMARY KEY,
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    submission_id   UUID         NOT NULL REFERENCES ar05_einvoice_submissions(submission_id),
    syntax          VARCHAR(32)  NOT NULL,
    content_type    VARCHAR(64)  NOT NULL,
    content         BYTEA        NOT NULL,
    sha256          VARCHAR(64)  NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    byte_len        INTEGER      NOT NULL CHECK (byte_len > 0),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (submission_id)
);

CREATE TABLE IF NOT EXISTS ar05_validation_results (
    result_id         UUID PRIMARY KEY,
    tenant_id         UUID         NOT NULL,
    legal_entity_id   UUID         NOT NULL,
    submission_id     UUID         NOT NULL REFERENCES ar05_einvoice_submissions(submission_id),
    run_no            INTEGER      NOT NULL CHECK (run_no >= 1),
    passed            BOOLEAN      NOT NULL,
    error_count       INTEGER      NOT NULL CHECK (error_count >= 0),
    errors            JSONB        NOT NULL DEFAULT '[]'::jsonb,
    profile_code      VARCHAR(128) NOT NULL,
    profile_version   VARCHAR(32)  NOT NULL,
    syntax            VARCHAR(32)  NOT NULL,
    dependencies      JSONB        NOT NULL DEFAULT '[]'::jsonb,
    content_hash      VARCHAR(64)  NOT NULL,
    artifact_hash     VARCHAR(64)  NOT NULL,
    validated_by      VARCHAR(255) NOT NULL,
    validated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (submission_id, run_no),
    CONSTRAINT ar05_validation_consistent CHECK ((passed AND error_count = 0) OR (NOT passed AND error_count > 0))
);

CREATE TABLE IF NOT EXISTS ar05_submission_attempts (
    external_attempt_id UUID PRIMARY KEY,
    tenant_id           UUID         NOT NULL,
    legal_entity_id     UUID         NOT NULL,
    submission_id       UUID         NOT NULL REFERENCES ar05_einvoice_submissions(submission_id),
    attempt_no          INTEGER      NOT NULL CHECK (attempt_no >= 1),
    provider            VARCHAR(64)  NOT NULL,
    artifact_hash       VARCHAR(64)  NOT NULL,
    request_digest      VARCHAR(64)  NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    parent_attempt_id   UUID REFERENCES ar05_submission_attempts(external_attempt_id),
    reconciliation_id   UUID,
    started_by          VARCHAR(255) NOT NULL,
    started_at          TIMESTAMPTZ  NOT NULL,
    UNIQUE (submission_id, attempt_no),
    CONSTRAINT ar05_retry_follows_reconciliation CHECK (parent_attempt_id IS NULL OR reconciliation_id IS NOT NULL)
);

CREATE TABLE IF NOT EXISTS ar05_attempt_results (
    result_id           UUID PRIMARY KEY,
    tenant_id           UUID         NOT NULL,
    legal_entity_id     UUID         NOT NULL,
    external_attempt_id UUID         NOT NULL REFERENCES ar05_submission_attempts(external_attempt_id),
    seq                 INTEGER      NOT NULL CHECK (seq >= 1),
    outcome             VARCHAR(24)  NOT NULL CHECK (outcome IN ('ACKNOWLEDGED','ACCEPTED','REJECTED','AMBIGUOUS','NOT_SENT','NOT_RECEIVED',
                                                               'RECEIVED','ADMIN_ACCEPTED','ADMIN_REJECTED','ADMIN_NOT_RECEIVED','SUPERSEDED','UNRESOLVED')),
    source              VARCHAR(16)  NOT NULL CHECK (source IN ('TRANSPORT','CALLBACK','RECONCILIATION','ADMIN','SYSTEM')),
    provider_ref        VARCHAR(255),
    response_digest     VARCHAR(64),
    detail              JSONB,
    recorded_by         VARCHAR(255) NOT NULL,
    recorded_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (external_attempt_id, seq)
);

CREATE TABLE IF NOT EXISTS ar05_attempt_index (
    external_attempt_id UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    legal_entity_id     UUID NOT NULL,
    submission_id       UUID NOT NULL,
    provider            VARCHAR(64) NOT NULL
);

CREATE TABLE IF NOT EXISTS ar05_acknowledgements (
    ack_id              UUID PRIMARY KEY,
    tenant_id           UUID         NOT NULL,
    legal_entity_id     UUID         NOT NULL,
    submission_id       UUID         NOT NULL REFERENCES ar05_einvoice_submissions(submission_id),
    external_attempt_id UUID         NOT NULL REFERENCES ar05_submission_attempts(external_attempt_id),
    provider            VARCHAR(64)  NOT NULL,
    provider_event_id   VARCHAR(128) NOT NULL,
    status              VARCHAR(16)  NOT NULL CHECK (status IN ('ACCEPTED','REJECTED','PROCESSING')),
    receipt_id          VARCHAR(256),
    provider_reference  VARCHAR(255),
    reason_code         VARCHAR(128),
    message             TEXT,
    occurred_at         TIMESTAMPTZ  NOT NULL,
    received_at         TIMESTAMPTZ  NOT NULL,
    source              VARCHAR(16)  NOT NULL CHECK (source IN ('CALLBACK','RECONCILIATION','ADMIN')),
    signature_verified  BOOLEAN      NOT NULL,
    signature_alg       VARCHAR(32),
    signature_digest    VARCHAR(64),
    callback_nonce      VARCHAR(128),
    callback_timestamp  TIMESTAMPTZ,
    body_digest         VARCHAR(64),
    disposition         VARCHAR(24)  NOT NULL CHECK (disposition IN ('APPLIED','NO_CHANGE','AFTER_TERMINAL','ILLEGAL_TRANSITION','MISSING_RECEIPT')),
    state_after         VARCHAR(16)  NOT NULL,
    recorded_by         VARCHAR(255) NOT NULL,
    CONSTRAINT ar05_callback_is_signed CHECK (source <> 'CALLBACK' OR (signature_verified AND signature_alg IS NOT NULL AND signature_digest IS NOT NULL)),
    CONSTRAINT uq_ar05_ack_event UNIQUE (tenant_id, external_attempt_id, provider_event_id)
);
CREATE INDEX IF NOT EXISTS idx_ar05_ack_submission ON ar05_acknowledgements (tenant_id, submission_id, received_at);

CREATE TABLE IF NOT EXISTS ar05_callback_nonces (
    provider  VARCHAR(64)  NOT NULL,
    nonce     VARCHAR(128) NOT NULL,
    seen_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, nonce)
);

CREATE TABLE IF NOT EXISTS ar05_reconciliations (
    reconciliation_id   UUID PRIMARY KEY,
    tenant_id           UUID         NOT NULL,
    legal_entity_id     UUID         NOT NULL,
    submission_id       UUID         NOT NULL REFERENCES ar05_einvoice_submissions(submission_id),
    external_attempt_id UUID         REFERENCES ar05_submission_attempts(external_attempt_id),
    method              VARCHAR(16)  NOT NULL CHECK (method IN ('QUERY_PROVIDER','ADMIN_EVIDENCE')),
    trigger             VARCHAR(16)  NOT NULL CHECK (trigger IN ('RECONCILE','RETRY')),
    provider_state      VARCHAR(16)  NOT NULL CHECK (provider_state IN ('RECEIVED','ACCEPTED','REJECTED','NOT_RECEIVED','UNKNOWN','ERROR')),
    state_before        VARCHAR(16)  NOT NULL,
    state_after         VARCHAR(16)  NOT NULL,
    provider_ref        VARCHAR(255),
    receipt_id          VARCHAR(256),
    reason_code         VARCHAR(128),
    evidence_id         VARCHAR(255),
    detail              JSONB,
    performed_by        VARCHAR(255) NOT NULL,
    performed_at        TIMESTAMPTZ  NOT NULL,
    CONSTRAINT ar05_admin_needs_evidence CHECK (method <> 'ADMIN_EVIDENCE' OR (reason_code IS NOT NULL AND evidence_id IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS ar05_submission_history (
    history_id          UUID PRIMARY KEY,
    tenant_id           UUID         NOT NULL,
    legal_entity_id     UUID         NOT NULL,
    submission_id       UUID         NOT NULL REFERENCES ar05_einvoice_submissions(submission_id),
    submission_version  INTEGER      NOT NULL,
    command             VARCHAR(64)  NOT NULL,
    state_before        VARCHAR(16),
    state_after         VARCHAR(16)  NOT NULL,
    actor_id            VARCHAR(255) NOT NULL,
    reason_code         VARCHAR(128),
    evidence_id         VARCHAR(255),
    correlation_id      VARCHAR(255) NOT NULL DEFAULT '',
    detail              JSONB,
    recorded_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (submission_id, submission_version)
);

CREATE TABLE IF NOT EXISTS ar05_security_events (
    event_id          UUID PRIMARY KEY,
    occurred_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    kind              VARCHAR(32)  NOT NULL,
    reason            VARCHAR(64)  NOT NULL,
    provider          VARCHAR(64),
    claimed_attempt   VARCHAR(64),
    tenant_id         UUID,
    remote_addr       VARCHAR(64),
    body_digest       VARCHAR(64),
    signature_present BOOLEAN      NOT NULL DEFAULT FALSE,
    detail            JSONB
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar05_artifacts','ar05_validation_results','ar05_submission_attempts','ar05_attempt_results','ar05_attempt_index',
                             'ar05_acknowledgements','ar05_callback_nonces','ar05_reconciliations','ar05_submission_history','ar05_security_events'] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t || '_append_only', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation()', t || '_append_only', t);
    END LOOP;
    -- Tenant tables: RLS enabled and FORCEd. The routing / replay / security tables are infrastructure (read before a tenant is known).
    FOREACH t IN ARRAY ARRAY['ar05_einvoice_submissions','ar05_artifacts','ar05_validation_results','ar05_submission_attempts','ar05_attempt_results',
                             'ar05_acknowledgements','ar05_reconciliations','ar05_submission_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
