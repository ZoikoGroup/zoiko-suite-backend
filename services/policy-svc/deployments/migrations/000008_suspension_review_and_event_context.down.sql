-- 000008_suspension_review_and_event_context.down.sql
ALTER TABLE outbox DROP COLUMN IF EXISTS jurisdiction_context;
ALTER TABLE policy_versions
    DROP COLUMN IF EXISTS suspension_review_outcome,
    DROP COLUMN IF EXISTS suspension_reviewed_at,
    DROP COLUMN IF EXISTS suspension_reviewed_by_principal_id,
    DROP COLUMN IF EXISTS suspended_by_principal_id;
