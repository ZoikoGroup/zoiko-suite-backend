-- 000003_lifecycle_versions_outbox.up.sql
--
-- 1. FIN-02 forecast lifecycle: CALCULATED -> REVIEWED -> APPROVED -> PUBLISHED
--    -> SUPERSEDED (plus ARCHIVED from the draft states), an optimistic
--    concurrency `version`, stored historical inputs for reproducible
--    recalculation, and maker/checker columns.
-- 2. AI-05: legal entity + version on assist jobs, an evaluation-approval step
--    on model releases, and a target-forecast snapshot on planner decisions.
-- 3. Generic idempotency (request fingerprint + resource id) for every write.
-- 4. Transactional outbox for domain events.

-- ── 1. Forecast lifecycle ───────────────────────────────────────────────────

ALTER TABLE forecast_models
    ADD COLUMN IF NOT EXISTS version                INT           NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS historical_data        JSONB         NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS growth_rate_adjustment NUMERIC(9,6)  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS prior_forecast_id      UUID          NULL REFERENCES forecast_models(id),
    ADD COLUMN IF NOT EXISTS created_by             VARCHAR(128)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS submitted_by           VARCHAR(128)  NULL,
    ADD COLUMN IF NOT EXISTS approved_by            VARCHAR(128)  NULL,
    ADD COLUMN IF NOT EXISTS approved_at            TIMESTAMPTZ   NULL,
    ADD COLUMN IF NOT EXISTS published_by           VARCHAR(128)  NULL,
    ADD COLUMN IF NOT EXISTS published_at           TIMESTAMPTZ   NULL;

UPDATE forecast_models SET status = 'CALCULATED' WHERE status IN ('ACTIVE', 'DRAFT');
ALTER TABLE forecast_models ALTER COLUMN status SET DEFAULT 'CALCULATED';
ALTER TABLE forecast_models ADD CONSTRAINT forecast_models_status_check
    CHECK (status IN ('CALCULATED', 'REVIEWED', 'APPROVED', 'PUBLISHED', 'SUPERSEDED', 'ARCHIVED'));
ALTER TABLE forecast_models ADD CONSTRAINT forecast_models_version_positive CHECK (version >= 1);

CREATE INDEX IF NOT EXISTS idx_forecast_models_page ON forecast_models(tenant_id, created_at DESC, id DESC);

-- RLS policies from 000001 had USING only; add WITH CHECK so a row can never be
-- written into a tenant other than the one declared for the transaction.
DROP POLICY IF EXISTS forecast_models_tenant_isolation ON forecast_models;
CREATE POLICY forecast_models_tenant_isolation ON forecast_models
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
DROP POLICY IF EXISTS forecast_projections_tenant_isolation ON forecast_projections;
CREATE POLICY forecast_projections_tenant_isolation ON forecast_projections
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Forecast state machine, enforced in the database as defence in depth (the
-- service enforces the same rules first and returns 409).
CREATE OR REPLACE FUNCTION enforce_forecast_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'forecasts cannot be deleted; archive them instead';
    END IF;

    IF OLD.status <> NEW.status THEN
        IF NOT (
               (OLD.status = 'CALCULATED' AND NEW.status IN ('REVIEWED', 'ARCHIVED'))
            OR (OLD.status = 'REVIEWED'   AND NEW.status IN ('APPROVED', 'ARCHIVED'))
            OR (OLD.status = 'APPROVED'   AND NEW.status = 'PUBLISHED')
            OR (OLD.status = 'PUBLISHED'  AND NEW.status = 'SUPERSEDED')
        ) THEN
            RAISE EXCEPTION 'invalid forecast transition from % to %', OLD.status, NEW.status;
        END IF;
    END IF;

    -- Calculated content may only change while the forecast is CALCULATED.
    IF OLD.status <> 'CALCULATED' AND (
           NEW.scenario_type          IS DISTINCT FROM OLD.scenario_type
        OR NEW.growth_rate_adjustment IS DISTINCT FROM OLD.growth_rate_adjustment
        OR NEW.historical_data        IS DISTINCT FROM OLD.historical_data
        OR NEW.horizon_periods        IS DISTINCT FROM OLD.horizon_periods
        OR NEW.algorithm_type         IS DISTINCT FROM OLD.algorithm_type
    ) THEN
        RAISE EXCEPTION 'forecast % is % and its calculated content is immutable', OLD.id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'forecast % identity columns are immutable', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_forecast_lifecycle ON forecast_models;
CREATE TRIGGER trigger_forecast_lifecycle
    BEFORE UPDATE OR DELETE ON forecast_models
    FOR EACH ROW EXECUTE FUNCTION enforce_forecast_lifecycle();

-- Projections of a non-CALCULATED forecast are frozen.
CREATE OR REPLACE FUNCTION enforce_projection_immutability()
RETURNS trigger AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT status INTO parent_status FROM forecast_models WHERE id = OLD.forecast_model_id;
    IF parent_status IS DISTINCT FROM 'CALCULATED' THEN
        RAISE EXCEPTION 'projections of a % forecast are immutable', parent_status;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_projection_immutability ON forecast_projections;
CREATE TRIGGER trigger_projection_immutability
    BEFORE UPDATE OR DELETE ON forecast_projections
    FOR EACH ROW EXECUTE FUNCTION enforce_projection_immutability();

-- ── 2. AI-05 additions ──────────────────────────────────────────────────────

ALTER TABLE forecast_assist_jobs
    ADD COLUMN IF NOT EXISTS legal_entity_id   VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS version           INT          NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS review_started_by VARCHAR(128) NULL;

CREATE INDEX IF NOT EXISTS idx_forecast_assist_jobs_page ON forecast_assist_jobs(tenant_id, created_at DESC, job_id DESC);

-- Re-declare the job lifecycle trigger so legal_entity_id is frozen too and
-- review_started_by is write-once.
CREATE OR REPLACE FUNCTION enforce_forecast_job_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'forecast assist jobs cannot be deleted';
    END IF;
    IF OLD.status IN ('Accepted', 'Rejected') THEN
        RAISE EXCEPTION 'forecast assist job % is % and immutable', OLD.job_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        IF NOT (
               (OLD.status = 'Queued'        AND NEW.status = 'Generated')
            OR (OLD.status = 'Generated'     AND NEW.status = 'PlannerReview')
            OR (OLD.status = 'PlannerReview' AND NEW.status IN ('Accepted', 'Rejected'))
        ) THEN
            RAISE EXCEPTION 'invalid forecast assist job transition from % to %', OLD.status, NEW.status;
        END IF;
    END IF;
    IF OLD.narrative IS NOT NULL AND NEW.narrative IS DISTINCT FROM OLD.narrative THEN
        RAISE EXCEPTION 'forecast assist job % narrative is write-once', OLD.job_id;
    END IF;
    IF OLD.review_started_by IS NOT NULL AND NEW.review_started_by IS DISTINCT FROM OLD.review_started_by THEN
        RAISE EXCEPTION 'forecast assist job % review_started_by is write-once', OLD.job_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.domain_name IS DISTINCT FROM OLD.domain_name
        OR NEW.model_provider IS DISTINCT FROM OLD.model_provider
        OR NEW.model_version IS DISTINCT FROM OLD.model_version
        OR NEW.plan_version IS DISTINCT FROM OLD.plan_version
        OR NEW.planning_purpose IS DISTINCT FROM OLD.planning_purpose
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION 'forecast assist job % may only have its status/narrative/version change', OLD.job_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE forecast_model_releases
    ADD COLUMN IF NOT EXISTS status               VARCHAR(24)  NOT NULL DEFAULT 'PendingEvaluation'
        CHECK (status IN ('PendingEvaluation', 'Approved')),
    ADD COLUMN IF NOT EXISTS version              INT          NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS evaluation_reference TEXT         NULL,
    ADD COLUMN IF NOT EXISTS approved_by          VARCHAR(128) NULL,
    ADD COLUMN IF NOT EXISTS approved_at          TIMESTAMPTZ  NULL;

CREATE INDEX IF NOT EXISTS idx_forecast_model_releases_page ON forecast_model_releases(tenant_id, created_at DESC, release_id DESC);

-- Model releases: identity is frozen; the only allowed change is the one-way
-- PendingEvaluation -> Approved evaluation sign-off.
CREATE OR REPLACE FUNCTION enforce_model_release_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'Approved' THEN
        RAISE EXCEPTION 'forecast model release % is approved and immutable', OLD.release_id;
    END IF;
    IF OLD.status <> NEW.status AND NOT (OLD.status = 'PendingEvaluation' AND NEW.status = 'Approved') THEN
        RAISE EXCEPTION 'invalid model release transition from % to %', OLD.status, NEW.status;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.domain_name IS DISTINCT FROM OLD.domain_name
        OR NEW.model_provider IS DISTINCT FROM OLD.model_provider
        OR NEW.model_version IS DISTINCT FROM OLD.model_version
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'forecast model release % identity columns are immutable', OLD.release_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_model_release_lifecycle ON forecast_model_releases;
CREATE TRIGGER trigger_model_release_lifecycle
    BEFORE UPDATE ON forecast_model_releases
    FOR EACH ROW EXECUTE FUNCTION enforce_model_release_lifecycle();

ALTER TABLE planner_decisions
    ADD COLUMN IF NOT EXISTS target_forecast_id      UUID        NULL,
    ADD COLUMN IF NOT EXISTS target_forecast_status  VARCHAR(16) NULL,
    ADD COLUMN IF NOT EXISTS target_forecast_version INT         NULL,
    ADD COLUMN IF NOT EXISTS note                    TEXT        NULL;

-- ── 3. Idempotency ──────────────────────────────────────────────────────────
-- The 000002 table is reused for every write. resource_id is filled in at the
-- end of the command, in the same transaction that claimed the key.

ALTER TABLE idempotency_keys ALTER COLUMN resource_id SET DEFAULT '';
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ── 4. Transactional outbox ─────────────────────────────────────────────────
-- Rows are inserted in the same transaction as the business change. The relay
-- (internal/outbox) publishes committed rows to Kafka and marks them published.
-- The relay is not tenant-scoped, so the RLS policy admits it when it declares
-- app.outbox_relay = 'on' for its own transaction; API transactions only ever
-- see their own tenant's rows.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_id        UUID         PRIMARY KEY,
    event_id         VARCHAR(64)  NOT NULL UNIQUE,
    event_type       VARCHAR(128) NOT NULL,
    schema_version   VARCHAR(16)  NOT NULL DEFAULT '1.0',
    tenant_id        VARCHAR(64)  NOT NULL,
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(128) NOT NULL,
    aggregate_version INT         NOT NULL DEFAULT 1,
    payload          JSONB        NOT NULL,
    payload_hash     VARCHAR(64)  NOT NULL,
    publish_state    VARCHAR(16)  NOT NULL DEFAULT 'Pending'
        CHECK (publish_state IN ('Pending', 'Published', 'Failed')),
    attempt_count    INT          NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_error       TEXT         NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at     TIMESTAMPTZ  NULL
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_pending ON outbox_events(publish_state, next_attempt_at)
    WHERE publish_state = 'Pending';

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_isolation ON outbox_events
    FOR ALL USING (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.outbox_relay', true) = 'on'
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.outbox_relay', true) = 'on'
    );
