-- 000003_lifecycle_versions_outbox.down.sql

DROP TABLE IF EXISTS outbox_events;

ALTER TABLE planner_decisions
    DROP COLUMN IF EXISTS target_forecast_id,
    DROP COLUMN IF EXISTS target_forecast_status,
    DROP COLUMN IF EXISTS target_forecast_version,
    DROP COLUMN IF EXISTS note;

DROP TRIGGER IF EXISTS trigger_model_release_lifecycle ON forecast_model_releases;
DROP FUNCTION IF EXISTS enforce_model_release_lifecycle();
DROP INDEX IF EXISTS idx_forecast_model_releases_page;
ALTER TABLE forecast_model_releases
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS evaluation_reference,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS approved_at;

-- Restore 000002's job trigger BEFORE dropping the columns 000003's version
-- reads (legal_entity_id, review_started_by); otherwise every UPDATE on
-- forecast_assist_jobs fails after a rollback.
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

DROP INDEX IF EXISTS idx_forecast_assist_jobs_page;
ALTER TABLE forecast_assist_jobs
    DROP COLUMN IF EXISTS legal_entity_id,
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS review_started_by;

DROP TRIGGER IF EXISTS trigger_projection_immutability ON forecast_projections;
DROP FUNCTION IF EXISTS enforce_projection_immutability();
DROP TRIGGER IF EXISTS trigger_forecast_lifecycle ON forecast_models;
DROP FUNCTION IF EXISTS enforce_forecast_lifecycle();

DROP INDEX IF EXISTS idx_forecast_models_page;
ALTER TABLE forecast_models DROP CONSTRAINT IF EXISTS forecast_models_status_check;
ALTER TABLE forecast_models DROP CONSTRAINT IF EXISTS forecast_models_version_positive;
ALTER TABLE forecast_models ALTER COLUMN status SET DEFAULT 'ACTIVE';
ALTER TABLE forecast_models
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS historical_data,
    DROP COLUMN IF EXISTS growth_rate_adjustment,
    DROP COLUMN IF EXISTS prior_forecast_id,
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS submitted_by,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS published_by,
    DROP COLUMN IF EXISTS published_at;
