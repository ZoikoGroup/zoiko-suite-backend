-- Schema for document-extraction-svc (AI-01, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Extraction Jobs: one extraction attempt against one document
-- version. Everything except status is immutable from creation — a
-- prompt/model upgrade never edits an existing job, it only ever
-- produces a brand new one (see reprocessed_from_job_id), which is
-- what keeps an already-accepted extraction's workflow from silently
-- changing underneath it.
CREATE TABLE IF NOT EXISTS extraction_jobs (
    job_id                            TEXT         PRIMARY KEY
        CHECK (job_id ~ '^exj_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                           VARCHAR(64)  NOT NULL,
    document_ref                         VARCHAR(256) NOT NULL,
    document_hash                          CHAR(64)     NOT NULL CHECK (document_hash ~ '^[0-9a-f]{64}$'),
    extraction_schema_id                     VARCHAR(128) NOT NULL,
    extraction_schema_version                  BIGINT       NOT NULL DEFAULT 1,
    review_confidence_threshold_bp               INT          NOT NULL CHECK (review_confidence_threshold_bp BETWEEN 0 AND 10000),
    classification                                 VARCHAR(64)  NOT NULL,
    residency_region                                 VARCHAR(64)  NOT NULL,
    status                                             VARCHAR(16)  NOT NULL DEFAULT 'Running'
        CHECK (status IN ('Queued','Running','ReviewRequired','Accepted','Rejected','Failed')),
    reprocessed_from_job_id                              TEXT         REFERENCES extraction_jobs(job_id),
    created_at                                             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                               VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_extraction_jobs_document ON extraction_jobs(tenant_id, document_ref);

-- Model Invocations: immutable provenance of the model call that
-- produced a job's candidates — one per job.
CREATE TABLE IF NOT EXISTS model_invocations (
    invocation_id                  TEXT         PRIMARY KEY
        CHECK (invocation_id ~ '^miv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                        VARCHAR(64)  NOT NULL,
    job_id                              TEXT         NOT NULL UNIQUE REFERENCES extraction_jobs(job_id),
    model_provider                        VARCHAR(128) NOT NULL,
    model_version                           VARCHAR(128) NOT NULL,
    prompt_version                            VARCHAR(128) NOT NULL,
    request_content_hash                        CHAR(64)     NOT NULL CHECK (request_content_hash ~ '^[0-9a-f]{64}$'),
    response_content_hash                         CHAR(64)     NOT NULL CHECK (response_content_hash ~ '^[0-9a-f]{64}$'),
    invoked_at                                      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Extraction Candidates: one extracted field per row. Evidentiary
-- columns (field_name/extracted_value/confidence_bp/protected) never
-- change after creation — only status (and the decision columns) may
-- move, exactly once, Pending -> Accepted or Pending -> Rejected (see
-- the trigger below).
CREATE TABLE IF NOT EXISTS extraction_candidates (
    candidate_id                  TEXT         PRIMARY KEY
        CHECK (candidate_id ~ '^exc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                       VARCHAR(64)  NOT NULL,
    job_id                             TEXT         NOT NULL REFERENCES extraction_jobs(job_id),
    field_name                           VARCHAR(256) NOT NULL,
    extracted_value                        TEXT         NOT NULL,
    confidence_bp                            INT          NOT NULL CHECK (confidence_bp BETWEEN 0 AND 10000),
    protected                                  BOOLEAN      NOT NULL DEFAULT FALSE,
    status                                       VARCHAR(16)  NOT NULL DEFAULT 'Pending' CHECK (status IN ('Pending','Accepted','Rejected')),
    created_at                                     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    decided_at                                       TIMESTAMPTZ,
    decided_by                                         VARCHAR(128),
    UNIQUE (job_id, field_name)
);

CREATE INDEX IF NOT EXISTS idx_extraction_candidates_job ON extraction_candidates(job_id, status);

-- Evidence Spans: points at exactly where in the original document a
-- candidate's value came from. Immutable, and never deleted when its
-- candidate is rejected — the doc's own "original document remains
-- available after candidate rejection" acceptance test.
CREATE TABLE IF NOT EXISTS evidence_spans (
    span_id                     TEXT         PRIMARY KEY
        CHECK (span_id ~ '^evs_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                     VARCHAR(64)  NOT NULL,
    candidate_id                    TEXT         NOT NULL UNIQUE REFERENCES extraction_candidates(candidate_id),
    page_number                       INT,
    start_offset                        INT          NOT NULL,
    end_offset                            INT          NOT NULL,
    snippet_text                            TEXT         NOT NULL,
    created_at                                TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (end_offset >= start_offset)
);

-- Extraction Decisions: append-only audit record of a human disposing
-- of a Pending candidate.
CREATE TABLE IF NOT EXISTS extraction_decisions (
    decision_id                  TEXT         PRIMARY KEY
        CHECK (decision_id ~ '^exd_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                      VARCHAR(64)  NOT NULL,
    candidate_id                     TEXT         NOT NULL REFERENCES extraction_candidates(candidate_id),
    decision                           VARCHAR(16)  NOT NULL CHECK (decision IN ('Accepted','Rejected')),
    reason                                TEXT,
    actor                                   VARCHAR(128) NOT NULL,
    decided_at                               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (decision <> 'Rejected' OR (reason IS NOT NULL AND reason <> ''))
);

-- Idempotency Keys: scoped per tenant from the start.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64)  NOT NULL,
    owner_scope           VARCHAR(32)  NOT NULL,
    principal_id             VARCHAR(128) NOT NULL,
    idempotency_key            VARCHAR(128) NOT NULL,
    operation                    VARCHAR(64)  NOT NULL,
    request_sha256                 VARCHAR(64)  NOT NULL,
    resource_id                      VARCHAR(128) NOT NULL,
    created_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type        VARCHAR(64)  NOT NULL,
    aggregate_id            VARCHAR(128) NOT NULL,
    event_type                VARCHAR(64)  NOT NULL,
    payload                      JSONB        NOT NULL,
    tenant_id                      VARCHAR(64),
    created_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at                       TIMESTAMPTZ,
    publish_attempts                     INT          NOT NULL DEFAULT 0,
    last_error                             TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE extraction_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE extraction_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY extraction_jobs_tenant_isolation ON extraction_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE model_invocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_invocations FORCE ROW LEVEL SECURITY;
CREATE POLICY model_invocations_tenant_isolation ON model_invocations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE extraction_candidates ENABLE ROW LEVEL SECURITY;
ALTER TABLE extraction_candidates FORCE ROW LEVEL SECURITY;
CREATE POLICY extraction_candidates_tenant_isolation ON extraction_candidates
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE evidence_spans ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_spans FORCE ROW LEVEL SECURITY;
CREATE POLICY evidence_spans_tenant_isolation ON evidence_spans
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE extraction_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE extraction_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY extraction_decisions_tenant_isolation ON extraction_decisions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Lifecycle triggers ───────────────────────────────────────────────────────

-- Extraction Jobs: forward-only status machine, fully terminal at
-- Accepted/Rejected/Failed (a prompt/model upgrade never reopens or
-- edits an existing job — see ReprocessWithVersion, which always
-- inserts a brand new job instead).
CREATE OR REPLACE FUNCTION enforce_job_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'extraction jobs cannot be deleted';
    END IF;
    IF OLD.status IN ('Accepted', 'Rejected', 'Failed') THEN
        RAISE EXCEPTION 'extraction job % is % and immutable', OLD.job_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Queued' THEN
                IF NEW.status <> 'Running' THEN
                    RAISE EXCEPTION 'invalid extraction job transition from Queued to %', NEW.status;
                END IF;
            WHEN 'Running' THEN
                IF NEW.status NOT IN ('ReviewRequired', 'Accepted', 'Failed') THEN
                    RAISE EXCEPTION 'invalid extraction job transition from Running to %', NEW.status;
                END IF;
            WHEN 'ReviewRequired' THEN
                IF NEW.status NOT IN ('Accepted', 'Rejected', 'Failed') THEN
                    RAISE EXCEPTION 'invalid extraction job transition from ReviewRequired to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown extraction job status %', OLD.status;
        END CASE;
    END IF;
    IF NEW.document_ref IS DISTINCT FROM OLD.document_ref
        OR NEW.document_hash IS DISTINCT FROM OLD.document_hash
        OR NEW.extraction_schema_id IS DISTINCT FROM OLD.extraction_schema_id
        OR NEW.extraction_schema_version IS DISTINCT FROM OLD.extraction_schema_version
        OR NEW.review_confidence_threshold_bp IS DISTINCT FROM OLD.review_confidence_threshold_bp
        OR NEW.classification IS DISTINCT FROM OLD.classification
        OR NEW.residency_region IS DISTINCT FROM OLD.residency_region
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION 'extraction job % may only have its status change', OLD.job_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_job_lifecycle
    BEFORE UPDATE OR DELETE ON extraction_jobs
    FOR EACH ROW EXECUTE FUNCTION enforce_job_lifecycle();

-- Model Invocations / Evidence Spans / Extraction Decisions: fully
-- immutable once written.
CREATE OR REPLACE FUNCTION reject_immutable_row()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable once written', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_model_invocations_immutable
    BEFORE UPDATE OR DELETE ON model_invocations
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_evidence_spans_immutable
    BEFORE UPDATE OR DELETE ON evidence_spans
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_extraction_decisions_immutable
    BEFORE UPDATE OR DELETE ON extraction_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

-- Extraction Candidates: the only legitimate transition is
-- Pending -> Accepted or Pending -> Rejected, together with
-- decided_at/decided_by being set in the same update — every
-- evidentiary column stays unchanged, and a decided candidate is fully
-- terminal.
CREATE OR REPLACE FUNCTION enforce_candidate_decision()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'extraction candidates cannot be deleted';
    END IF;
    IF OLD.status <> 'Pending' THEN
        RAISE EXCEPTION 'extraction candidate % is % and immutable', OLD.candidate_id, OLD.status;
    END IF;
    IF NEW.status NOT IN ('Accepted', 'Rejected') THEN
        RAISE EXCEPTION 'extraction candidates may only transition Pending -> Accepted or Pending -> Rejected';
    END IF;
    IF NEW.decided_at IS NULL OR NEW.decided_by IS NULL THEN
        RAISE EXCEPTION 'deciding a candidate requires decided_at and decided_by';
    END IF;
    IF NEW.job_id IS DISTINCT FROM OLD.job_id
        OR NEW.field_name IS DISTINCT FROM OLD.field_name
        OR NEW.extracted_value IS DISTINCT FROM OLD.extracted_value
        OR NEW.confidence_bp IS DISTINCT FROM OLD.confidence_bp
        OR NEW.protected IS DISTINCT FROM OLD.protected
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'extraction candidate % may only have status/decided_at/decided_by change', OLD.candidate_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_candidate_decision
    BEFORE UPDATE OR DELETE ON extraction_candidates
    FOR EACH ROW EXECUTE FUNCTION enforce_candidate_decision();
