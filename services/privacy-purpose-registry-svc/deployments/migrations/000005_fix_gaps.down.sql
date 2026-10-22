-- 000005_fix_gaps.down.sql
DROP TABLE IF EXISTS event_outbox;
DROP INDEX IF EXISTS uq_activity_versions_one_active;

ALTER TABLE processing_activity_versions
    DROP COLUMN IF EXISTS approved_by_principal_id,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS rejected_by_principal_id,
    DROP COLUMN IF EXISTS rejected_at,
    DROP COLUMN IF EXISTS activated_by_principal_id,
    DROP COLUMN IF EXISTS activated_at;

ALTER TABLE purpose_versions
    DROP COLUMN IF EXISTS published_by_principal_id,
    DROP COLUMN IF EXISTS published_at;
