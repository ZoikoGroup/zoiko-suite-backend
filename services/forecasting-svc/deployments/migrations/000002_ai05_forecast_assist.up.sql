-- 000002_ai05_forecast_assist.up.sql
-- Adds the ZS-SVC-N-001 AI-05 (Forecast Assist) governed advisory
-- workflow alongside this service's existing forecast_models/
-- forecast_projections computation engine, which is untouched. The two
-- surfaces are independent: forecast_models/forecast_projections
-- remain the Phase 6 forecasting-engine feature (approved forecast
-- values), while the tables below add a governed suggestion/review
-- workflow that can never mutate them. "Accepted suggestions create
-- normal FIN candidates, never accounting entries" — nothing in this
-- layer writes to forecast_models, forecast_projections, or any
-- accounting table, under any outcome.

-- Forecast Model Releases: seller-managed model/provider registration
-- per (domain, provider, version). Registering a release IS the
-- evaluation-passed signal — SuggestDrivers/SuggestRange/
-- GenerateNarrative all refuse outright against an unregistered
-- combination: "model/provider change requires evaluation before
-- material use."
CREATE TABLE IF NOT EXISTS forecast_model_releases (
    release_id       TEXT         PRIMARY KEY
        CHECK (release_id ~ '^fmr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id        VARCHAR(64)  NOT NULL,
    domain_name      VARCHAR(128) NOT NULL,
    model_provider   VARCHAR(128) NOT NULL,
    model_version    VARCHAR(128) NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by       VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, domain_name, model_provider, model_version)
);

-- Forecast Assist Jobs: one per planning assist session. Forward-only
-- lifecycle: Queued -> Generated -> PlannerReview -> (Accepted|
-- Rejected), then fully terminal. narrative is write-once: it may move
-- from NULL to a value, never change again.
CREATE TABLE IF NOT EXISTS forecast_assist_jobs (
    job_id              TEXT         PRIMARY KEY
        CHECK (job_id ~ '^faj_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id           VARCHAR(64)  NOT NULL,
    domain_name         VARCHAR(128) NOT NULL,
    model_provider      VARCHAR(128) NOT NULL,
    model_version       VARCHAR(128) NOT NULL,
    plan_version        VARCHAR(128) NOT NULL,
    planning_purpose    VARCHAR(128) NOT NULL,
    narrative           TEXT,
    status              VARCHAR(16)  NOT NULL DEFAULT 'Queued'
        CHECK (status IN ('Queued','Generated','PlannerReview','Accepted','Rejected')),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by          VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_forecast_assist_jobs_status ON forecast_assist_jobs(tenant_id, status);

-- Suggested Drivers: immutable once written.
CREATE TABLE IF NOT EXISTS suggested_drivers (
    driver_id        TEXT         PRIMARY KEY
        CHECK (driver_id ~ '^sdr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id        VARCHAR(64)  NOT NULL,
    job_id           TEXT         NOT NULL REFERENCES forecast_assist_jobs(job_id),
    driver_name      VARCHAR(128) NOT NULL,
    suggested_value  NUMERIC(18,4) NOT NULL,
    rationale        TEXT         NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_suggested_drivers_job ON suggested_drivers(tenant_id, job_id);

-- Suggested Ranges: immutable once written.
CREATE TABLE IF NOT EXISTS suggested_ranges (
    range_id          TEXT         PRIMARY KEY
        CHECK (range_id ~ '^srg_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id         VARCHAR(64)  NOT NULL,
    job_id            TEXT         NOT NULL REFERENCES forecast_assist_jobs(job_id),
    period_label      VARCHAR(64)  NOT NULL,
    low_value         NUMERIC(18,4) NOT NULL,
    high_value        NUMERIC(18,4) NOT NULL,
    confidence_note   TEXT         NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (high_value >= low_value)
);

CREATE INDEX IF NOT EXISTS idx_suggested_ranges_job ON suggested_ranges(tenant_id, job_id);

-- Evidence References: immutable once written. Supports drivers,
-- ranges and the narrative alike.
CREATE TABLE IF NOT EXISTS evidence_references (
    evidence_id    TEXT         PRIMARY KEY
        CHECK (evidence_id ~ '^evr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id      VARCHAR(64)  NOT NULL,
    job_id         TEXT         NOT NULL REFERENCES forecast_assist_jobs(job_id),
    source_ref     VARCHAR(256) NOT NULL,
    description    TEXT         NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_evidence_references_job ON evidence_references(tenant_id, job_id);

-- Planner Decisions: append-only, exactly one row per job — the
-- terminal decision.
CREATE TABLE IF NOT EXISTS planner_decisions (
    decision_id    TEXT         PRIMARY KEY
        CHECK (decision_id ~ '^pld_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id      VARCHAR(64)  NOT NULL,
    job_id         TEXT         NOT NULL UNIQUE REFERENCES forecast_assist_jobs(job_id),
    decision       VARCHAR(16)  NOT NULL CHECK (decision IN ('Accepted','Rejected')),
    reason         TEXT,
    actor          VARCHAR(128) NOT NULL,
    decided_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (decision <> 'Rejected' OR (reason IS NOT NULL AND reason <> ''))
);

-- Idempotency Keys: did not previously exist in this service. Used by
-- the new AI-05 commands only.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id       VARCHAR(64)  NOT NULL,
    owner_scope     VARCHAR(32)  NOT NULL,
    principal_id    VARCHAR(128) NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    operation       VARCHAR(64)  NOT NULL,
    request_sha256  VARCHAR(64)  NOT NULL,
    resource_id     VARCHAR(128) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE forecast_model_releases ENABLE ROW LEVEL SECURITY;
ALTER TABLE forecast_model_releases FORCE ROW LEVEL SECURITY;
CREATE POLICY forecast_model_releases_tenant_isolation ON forecast_model_releases
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE forecast_assist_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE forecast_assist_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY forecast_assist_jobs_tenant_isolation ON forecast_assist_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE suggested_drivers ENABLE ROW LEVEL SECURITY;
ALTER TABLE suggested_drivers FORCE ROW LEVEL SECURITY;
CREATE POLICY suggested_drivers_tenant_isolation ON suggested_drivers
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE suggested_ranges ENABLE ROW LEVEL SECURITY;
ALTER TABLE suggested_ranges FORCE ROW LEVEL SECURITY;
CREATE POLICY suggested_ranges_tenant_isolation ON suggested_ranges
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE evidence_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_references FORCE ROW LEVEL SECURITY;
CREATE POLICY evidence_references_tenant_isolation ON evidence_references
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE planner_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE planner_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY planner_decisions_tenant_isolation ON planner_decisions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Harden the pre-existing tables with the same tenant-isolation
-- guarantee the rest of this platform relies on.
ALTER TABLE forecast_models FORCE ROW LEVEL SECURITY;
ALTER TABLE forecast_projections FORCE ROW LEVEL SECURITY;

-- ── Lifecycle triggers ───────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION reject_delete()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows cannot be deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_forecast_model_releases_no_delete
    BEFORE DELETE ON forecast_model_releases
    FOR EACH ROW EXECUTE FUNCTION reject_delete();

CREATE OR REPLACE FUNCTION reject_immutable_row()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable once written', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_suggested_drivers_immutable
    BEFORE UPDATE OR DELETE ON suggested_drivers
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_suggested_ranges_immutable
    BEFORE UPDATE OR DELETE ON suggested_ranges
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_evidence_references_immutable
    BEFORE UPDATE OR DELETE ON evidence_references
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_planner_decisions_immutable
    BEFORE UPDATE OR DELETE ON planner_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

-- Forecast Assist Jobs: forward-only status machine, fully terminal at
-- Accepted/Rejected. narrative is write-once (NULL -> value only).
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
        CASE OLD.status
            WHEN 'Queued' THEN
                IF NEW.status <> 'Generated' THEN
                    RAISE EXCEPTION 'invalid forecast assist job transition from Queued to %', NEW.status;
                END IF;
            WHEN 'Generated' THEN
                IF NEW.status <> 'PlannerReview' THEN
                    RAISE EXCEPTION 'invalid forecast assist job transition from Generated to %', NEW.status;
                END IF;
            WHEN 'PlannerReview' THEN
                IF NEW.status NOT IN ('Accepted', 'Rejected') THEN
                    RAISE EXCEPTION 'invalid forecast assist job transition from PlannerReview to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown forecast assist job status %', OLD.status;
        END CASE;
    END IF;
    IF OLD.narrative IS NOT NULL AND NEW.narrative IS DISTINCT FROM OLD.narrative THEN
        RAISE EXCEPTION 'forecast assist job % narrative is write-once', OLD.job_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.domain_name IS DISTINCT FROM OLD.domain_name
        OR NEW.model_provider IS DISTINCT FROM OLD.model_provider
        OR NEW.model_version IS DISTINCT FROM OLD.model_version
        OR NEW.plan_version IS DISTINCT FROM OLD.plan_version
        OR NEW.planning_purpose IS DISTINCT FROM OLD.planning_purpose
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION 'forecast assist job % may only have its status/narrative change', OLD.job_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_forecast_job_lifecycle
    BEFORE UPDATE OR DELETE ON forecast_assist_jobs
    FOR EACH ROW EXECUTE FUNCTION enforce_forecast_job_lifecycle();
