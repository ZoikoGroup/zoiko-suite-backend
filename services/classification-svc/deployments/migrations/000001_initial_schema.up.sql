-- Schema for classification-svc (AI-02, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Model Releases: seller-managed drift gate per (taxonomy, model
-- provider, model version). Mutable in place, same idiom as
-- search_policies/reconciliation_definitions elsewhere in this
-- platform — Classify refuses outright, before any job exists, when an
-- invocation's own observed drift exceeds max_drift_bp.
CREATE TABLE IF NOT EXISTS model_releases (
    model_release_id             TEXT         PRIMARY KEY
        CHECK (model_release_id ~ '^cmr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                      VARCHAR(64)  NOT NULL,
    taxonomy_id                      VARCHAR(128) NOT NULL,
    model_provider                     VARCHAR(128) NOT NULL,
    model_version                        VARCHAR(128) NOT NULL,
    max_drift_bp                           INT          NOT NULL CHECK (max_drift_bp BETWEEN 0 AND 10000),
    created_at                               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                 VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, taxonomy_id, model_provider, model_version)
);

-- Classification Jobs: one classification attempt against one object.
-- Everything except status is immutable from creation. A protected job
-- always requires review regardless of confidence (see the trigger and
-- store logic) — "high confidence does not bypass protected review."
CREATE TABLE IF NOT EXISTS classification_jobs (
    job_id                    TEXT         PRIMARY KEY
        CHECK (job_id ~ '^clj_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    object_ref                    VARCHAR(256) NOT NULL,
    object_type                     VARCHAR(64)  NOT NULL,
    taxonomy_id                       VARCHAR(128) NOT NULL,
    taxonomy_version                    BIGINT       NOT NULL DEFAULT 1,
    model_provider                        VARCHAR(128) NOT NULL,
    model_version                           VARCHAR(128) NOT NULL,
    protected                                 BOOLEAN      NOT NULL DEFAULT FALSE,
    status                                      VARCHAR(16)  NOT NULL DEFAULT 'Scored'
        CHECK (status IN ('Queued','Scored','ReviewRequired','Accepted','Rejected')),
    created_at                                    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                      VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_classification_jobs_object ON classification_jobs(tenant_id, object_ref);

-- Feature Snapshots: immutable evidence of the features a job's
-- candidates were scored from — one per job.
CREATE TABLE IF NOT EXISTS feature_snapshots (
    snapshot_id                TEXT         PRIMARY KEY
        CHECK (snapshot_id ~ '^cfs_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                    VARCHAR(64)  NOT NULL,
    job_id                          TEXT         NOT NULL UNIQUE REFERENCES classification_jobs(job_id),
    features                          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    content_hash                        CHAR(64)     NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    created_at                            TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Classification Candidates: one ranked label suggestion per row,
-- immutable evidence. rank = 1 is the top/actionable suggestion.
CREATE TABLE IF NOT EXISTS classification_candidates (
    candidate_id               TEXT         PRIMARY KEY
        CHECK (candidate_id ~ '^clc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                    VARCHAR(64)  NOT NULL,
    job_id                          TEXT         NOT NULL REFERENCES classification_jobs(job_id),
    rank                              INT          NOT NULL CHECK (rank >= 1),
    label                               VARCHAR(256) NOT NULL,
    confidence_bp                        INT          NOT NULL CHECK (confidence_bp BETWEEN 0 AND 10000),
    created_at                             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (job_id, rank)
);

-- Classification Decisions: append-only, exactly one row per job — the
-- terminal decision, whether auto-recorded by Classify or recorded by
-- AcceptSuggestion/RejectSuggestion/OverrideWithReason.
CREATE TABLE IF NOT EXISTS classification_decisions (
    decision_id                  TEXT         PRIMARY KEY
        CHECK (decision_id ~ '^cld_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                      VARCHAR(64)  NOT NULL,
    job_id                            TEXT         NOT NULL UNIQUE REFERENCES classification_jobs(job_id),
    decision                            VARCHAR(16)  NOT NULL CHECK (decision IN ('Accepted','Rejected','Overridden')),
    final_label                           VARCHAR(256),
    reason                                   TEXT,
    actor                                      VARCHAR(128) NOT NULL,
    decided_at                                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (decision <> 'Rejected' OR (reason IS NOT NULL AND reason <> '')),
    CHECK (decision <> 'Overridden' OR (reason IS NOT NULL AND reason <> '' AND final_label IS NOT NULL)),
    CHECK (decision = 'Rejected' OR final_label IS NOT NULL)
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

ALTER TABLE model_releases ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_releases FORCE ROW LEVEL SECURITY;
CREATE POLICY model_releases_tenant_isolation ON model_releases
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE classification_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE classification_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY classification_jobs_tenant_isolation ON classification_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE feature_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE feature_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY feature_snapshots_tenant_isolation ON feature_snapshots
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE classification_candidates ENABLE ROW LEVEL SECURITY;
ALTER TABLE classification_candidates FORCE ROW LEVEL SECURITY;
CREATE POLICY classification_candidates_tenant_isolation ON classification_candidates
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE classification_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE classification_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY classification_decisions_tenant_isolation ON classification_decisions
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

-- Model Releases: mutable-in-place reference data. No DELETE — a
-- release stays visible as governance history even if superseded by a
-- later UPSERT-style write from RegisterModelRelease.
CREATE OR REPLACE FUNCTION reject_delete()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows cannot be deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_model_releases_no_delete
    BEFORE DELETE ON model_releases
    FOR EACH ROW EXECUTE FUNCTION reject_delete();

-- Classification Jobs: forward-only status machine, fully terminal at
-- Accepted/Rejected.
CREATE OR REPLACE FUNCTION enforce_job_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'classification jobs cannot be deleted';
    END IF;
    IF OLD.status IN ('Accepted', 'Rejected') THEN
        RAISE EXCEPTION 'classification job % is % and immutable', OLD.job_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Queued' THEN
                IF NEW.status <> 'Scored' THEN
                    RAISE EXCEPTION 'invalid classification job transition from Queued to %', NEW.status;
                END IF;
            WHEN 'Scored' THEN
                IF NEW.status NOT IN ('ReviewRequired', 'Accepted') THEN
                    RAISE EXCEPTION 'invalid classification job transition from Scored to %', NEW.status;
                END IF;
            WHEN 'ReviewRequired' THEN
                IF NEW.status NOT IN ('Accepted', 'Rejected') THEN
                    RAISE EXCEPTION 'invalid classification job transition from ReviewRequired to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown classification job status %', OLD.status;
        END CASE;
    END IF;
    IF NEW.object_ref IS DISTINCT FROM OLD.object_ref
        OR NEW.object_type IS DISTINCT FROM OLD.object_type
        OR NEW.taxonomy_id IS DISTINCT FROM OLD.taxonomy_id
        OR NEW.taxonomy_version IS DISTINCT FROM OLD.taxonomy_version
        OR NEW.model_provider IS DISTINCT FROM OLD.model_provider
        OR NEW.model_version IS DISTINCT FROM OLD.model_version
        OR NEW.protected IS DISTINCT FROM OLD.protected
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION 'classification job % may only have its status change', OLD.job_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_job_lifecycle
    BEFORE UPDATE OR DELETE ON classification_jobs
    FOR EACH ROW EXECUTE FUNCTION enforce_job_lifecycle();

-- Feature Snapshots / Classification Candidates / Classification
-- Decisions: fully immutable once written.
CREATE OR REPLACE FUNCTION reject_immutable_row()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable once written', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_feature_snapshots_immutable
    BEFORE UPDATE OR DELETE ON feature_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_classification_candidates_immutable
    BEFORE UPDATE OR DELETE ON classification_candidates
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_classification_decisions_immutable
    BEFORE UPDATE OR DELETE ON classification_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();
